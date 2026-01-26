/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package task

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

type diskOptions struct {
	name     string
	source   string
	fsType   string
	readOnly bool
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio.
// Returns the transformed mounts and the number of disks allocated.
func transformMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount) ([]*types.Mount, byte, error) {
	var (
		disks    byte = 'a'
		addDisks []diskOptions
		am       []*types.Mount
		err      error
	)

	for _, m := range ms {
		switch m.Type {
		case "overlay", "format/overlay", "format/mkdir/overlay":
			var (
				wdi = -1
				udi = -1
			)
			for i, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					udi = i
				} else if strings.HasPrefix(opt, "workdir=") {
					wdi = i
				}
				// TODO: Handle virtio for lowers?
			}
			if wdi > -1 && udi > -1 {
				//
				// If any upperdir or workdir isn't transformed, they both
				// should fall back to virtiofs passthroughfs.  But...
				//
				if !strings.Contains(m.Options[wdi], "{{") ||
					!strings.Contains(m.Options[udi], "{{") {
					// Having the upper as virtiofs may return invalid argument, avoid
					// transforming and attempt to perform the mounts on the host if
					// supported.
					return nil, 0, fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			am = append(am, m)
		default:
			// Check if source is a file - if so, treat as block device mount
			if fi, err := os.Stat(m.Source); err == nil && !fi.IsDir() {
				disk := fmt.Sprintf("disk-%d-%s", disks, id)
				// virtiofs implementation has a limit of 36 characters for the tag
				if len(disk) > 36 {
					disk = disk[:36]
				}
				// Check if read-only is specified in options
				readOnly := hasReadOnlyOption(m.Options)
				addDisks = append(addDisks, diskOptions{
					name:     disk,
					source:   m.Source,
					fsType:   m.Type,
					readOnly: readOnly,
				})
				am = append(am, &types.Mount{
					Type:    m.Type,
					Source:  fmt.Sprintf("/dev/vd%c", disks),
					Target:  m.Target,
					Options: filterOptions(m.Options),
				})
				disks++
			} else {
				am = append(am, m)
			}
		}
	}

	if len(addDisks) > 10 {
		return nil, 0, fmt.Errorf("exceeded maximum virtio disk count: %d > 10: %w", len(addDisks), errdefs.ErrNotImplemented)
	}

	for _, do := range addDisks {
		var opts []vm.MountOpt
		if do.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}
		if err := vmi.AddDisk(ctx, do.name, do.source, opts...); err != nil {
			return nil, 0, err
		}
	}

	// Return the number of disks used (disks - 'a' gives the count)
	diskCount := disks - 'a'
	return am, diskCount, err
}

func filterOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		switch o {
		case "loop":
		default:
			filtered = append(filtered, o)
		}
	}
	return filtered
}

func hasReadOnlyOption(options []string) bool {
	for _, opt := range options {
		if opt == "ro" {
			return true
		}
	}
	return false
}

// specMount represents a mount from the OCI spec that needs VM setup.
// It supports different mount types:
//   - bind: transformed to virtiofs shares
//   - block device filesystems (ext4, erofs, xfs, etc.): passed as virtio block devices
type specMount struct {
	// mountType is the original mount type from the OCI spec (bind, or filesystem type)
	mountType string
	// isBlockDevice indicates this is a block device backed filesystem
	isBlockDevice bool
	// tag is the virtio-fs tag (for bind mounts) or disk identifier
	tag string
	// hostSrc is the source path on the host
	hostSrc string
	// vmTarget is the mount target path in the VM (for pre-mounting)
	vmTarget string
	// vmSource is the source in the VM (device path for block devices)
	vmSource string
	// options are mount options
	options []string
	// readOnly indicates if the mount should be read-only
	readOnly bool
}

type bindMounter struct {
	mounts []specMount
	// diskCounter tracks the next available virtio disk letter
	diskCounter byte
}

// SetDiskOffset sets the starting disk letter offset.
// This should be called before FromBundle if there are rootfs disks
// that have already been allocated.
func (bm *bindMounter) SetDiskOffset(offset byte) {
	bm.diskCounter = 'a' + offset
}

// DiskCount returns the number of disks allocated by this mounter.
func (bm *bindMounter) DiskCount() byte {
	count := byte(0)
	for _, m := range bm.mounts {
		if m.isBlockDevice {
			count++
		}
	}
	return count
}

// diskLetter returns the next available disk letter and increments the counter
func (bm *bindMounter) nextDiskLetter() byte {
	if bm.diskCounter == 0 {
		bm.diskCounter = 'a'
	}
	letter := bm.diskCounter
	bm.diskCounter++
	return letter
}

func (bm *bindMounter) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		// Skip mounts with template placeholders - they will be resolved VM-side
		if hasTemplatePlaceholder(m.Source) {
			log.G(ctx).WithField("mount", m).Debug("skipping mount with template placeholder in source")
			continue
		}

		mountType := m.Type
		// Strip format/ prefix - format transformations are handled VM-side
		mountType = strings.TrimPrefix(mountType, "format/")
		// Strip mkdir/ prefix - mkdir is handled VM-side
		mountType = strings.TrimPrefix(mountType, "mkdir/")

		switch mountType {
		case "bind":
			if err := bm.addBindMount(ctx, b, i, m); err != nil {
				return err
			}
		default:
			// Check if source is a file - if so, treat as block device mount
			if fi, err := os.Stat(m.Source); err == nil && !fi.IsDir() {
				if err := bm.addBlockDeviceMount(ctx, b, i, m, mountType); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// hasTemplatePlaceholder checks if a path contains template placeholders like {{ mount X }}
func hasTemplatePlaceholder(path string) bool {
	return strings.Contains(path, "{{")
}

func (bm *bindMounter) addBindMount(ctx context.Context, b *bundle.Bundle, i int, m specs.Mount) error {
	log.G(ctx).WithField("mount", m).Debug("transforming bind mount into a virtiofs mount")

	fi, err := os.Stat(m.Source)
	if err != nil {
		return fmt.Errorf("failed to stat bind mount source %s: %w", m.Source, err)
	}

	hash := sha256.Sum256([]byte(m.Destination))
	tag := fmt.Sprintf("bind-%x", hash[:8])
	vmTarget := "/mnt/" + tag

	// For files, share the parent directory via virtiofs since virtiofs
	// operates on directories. The spec source points to the file within
	// the mounted directory.
	hostSrc := m.Source
	specSrc := vmTarget
	if !fi.IsDir() {
		hostSrc = filepath.Dir(m.Source)
		specSrc = filepath.Join(vmTarget, filepath.Base(m.Source))
	}

	transformed := specMount{
		mountType: "bind",
		tag:       tag,
		hostSrc:   hostSrc,
		vmTarget:  vmTarget,
	}

	bm.mounts = append(bm.mounts, transformed)
	b.Spec.Mounts[i].Source = specSrc

	return nil
}

// addBlockDeviceMount handles any filesystem type backed by a block device (image file).
// This includes ext4, erofs, xfs, btrfs, squashfs, and any other filesystem
// that can be mounted from an image file.
func (bm *bindMounter) addBlockDeviceMount(ctx context.Context, b *bundle.Bundle, i int, m specs.Mount, fsType string) error {
	log.G(ctx).WithField("mount", m).WithField("fsType", fsType).Debug("transforming block device mount")

	hash := sha256.Sum256([]byte(m.Destination))
	tag := fmt.Sprintf("blk-%x", hash[:8])
	diskLetter := bm.nextDiskLetter()
	vmDevice := fmt.Sprintf("/dev/vd%c", diskLetter)
	vmTarget := "/mnt/" + tag

	transformed := specMount{
		mountType:     fsType,
		isBlockDevice: true,
		tag:           tag,
		hostSrc:       m.Source,
		vmTarget:      vmTarget,
		vmSource:      vmDevice,
		options:       filterSpecOptions(m.Options),
		readOnly:      hasReadOnlyOption(m.Options),
	}

	bm.mounts = append(bm.mounts, transformed)
	// Update the spec to use the VM target path
	b.Spec.Mounts[i].Source = vmTarget
	// Change the type back to bind since the filesystem will be pre-mounted in the VM
	b.Spec.Mounts[i].Type = "bind"
	// Remove filesystem-specific options that don't apply to bind mounts
	b.Spec.Mounts[i].Options = filterBindOptions(m.Options)

	return nil
}

// filterSpecOptions filters out options that are not applicable for the
// filesystem mount inside the VM
func filterSpecOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		switch o {
		case "loop", "bind", "rbind", "private", "rprivate", "shared", "rshared", "slave", "rslave":
			// Skip OCI-specific or loop mount options
		default:
			filtered = append(filtered, o)
		}
	}
	return filtered
}

// filterBindOptions filters options to only keep those valid for bind mounts
func filterBindOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		switch o {
		case "ro", "rw", "bind", "rbind", "private", "rprivate", "shared", "rshared", "slave", "rslave":
			filtered = append(filtered, o)
		}
	}
	// Ensure bind is present
	hasBind := false
	for _, o := range filtered {
		if o == "bind" || o == "rbind" {
			hasBind = true
			break
		}
	}
	if !hasBind {
		filtered = append([]string{"bind"}, filtered...)
	}
	return filtered
}

func (bm *bindMounter) SetupVM(ctx context.Context, vmi vm.Instance) error {
	for _, m := range bm.mounts {
		if m.isBlockDevice {
			var opts []vm.MountOpt
			if m.readOnly {
				opts = append(opts, vm.WithReadOnly())
			}
			if err := vmi.AddDisk(ctx, m.tag, m.hostSrc, opts...); err != nil {
				return fmt.Errorf("failed to add disk for %s mount %s: %w", m.mountType, m.tag, err)
			}
		} else {
			// bind mount via virtiofs
			if err := vmi.AddFS(ctx, m.tag, m.hostSrc); err != nil {
				return fmt.Errorf("failed to add virtiofs for bind mount %s: %w", m.tag, err)
			}
		}
	}
	return nil
}

func (bm *bindMounter) InitArgs() []string {
	args := make([]string, 0, len(bm.mounts))
	for _, m := range bm.mounts {
		// Format: -mount=type:source:target[:options]
		if m.isBlockDevice {
			// For block device mounts: -mount=fstype:/dev/vdX:vmTarget[:options]
			arg := fmt.Sprintf("-mount=%s:%s:%s", m.mountType, m.vmSource, m.vmTarget)
			if len(m.options) > 0 {
				arg += ":" + strings.Join(m.options, ",")
			}
			args = append(args, arg)
		} else {
			// For virtiofs bind mounts: -mount=virtiofs:tag:vmTarget
			args = append(args, "-mount=virtiofs:"+m.tag+":"+m.vmTarget)
		}
	}
	return args
}
