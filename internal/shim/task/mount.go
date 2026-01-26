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
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

type diskOptions struct {
	name     string
	source   string
	readOnly bool
}

// activeMount tracks a mount that has been processed and can be referenced
// by subsequent mounts via templating.
type activeMount struct {
	// Source is the original source path
	Source string
	// MountPoint is where this mount is accessible in the VM
	MountPoint string
}

// transformMounts does not perform any local mounts but transforms
// the mounts to be used inside the VM via virtio
func transformMounts(ctx context.Context, vmi vm.Instance, id string, ms []*types.Mount) ([]*types.Mount, error) {
	var (
		disks    byte = 'a'
		addDisks []diskOptions
		am       []*types.Mount
		active   []activeMount
		err      error
	)

	// mdir is used for intermediate mount points in the VM
	const mdir = "/run/containerd/mounts"

	for i, m := range ms {
		mountType := m.Type

		// Handle mkfs/ prefix - create filesystem on the source image
		if t, ok := strings.CutPrefix(mountType, "mkfs/"); ok {
			mountType = t
			if err := runMkfs(ctx, t, m.Source, m.Options); err != nil {
				return nil, fmt.Errorf("mkfs %s on %s: %w", t, m.Source, err)
			}
		}

		// Handle format/ prefix - expand templates in source, target, and options
		if t, ok := strings.CutPrefix(mountType, "format/"); ok {
			mountType = t
			m, err = formatMount(m, active)
			if err != nil {
				return nil, err
			}
		}

		// Handle mkdir/ prefix - create directories specified in options
		if t, ok := strings.CutPrefix(mountType, "mkdir/"); ok {
			mountType = t
			m, err = mkdirMount(m)
			if err != nil {
				return nil, err
			}
		}

		// Determine the mount point for this mount.
		// Intermediate mounts (not the last one) get a temporary mount point.
		// The last mount uses the rootfs target.
		var mountPoint string
		if m.Target != "" {
			mountPoint = m.Target
		} else if i < len(ms)-1 {
			// Intermediate mount - assign a temporary mount point in the VM
			mountPoint = fmt.Sprintf("%s/%d", mdir, i)
		} else {
			// Last mount without explicit target - this will be the rootfs
			mountPoint = "/rootfs"
		}

		switch mountType {
		case "erofs":
			disk := fmt.Sprintf("disk-%c-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			addDisks = append(addDisks, diskOptions{
				name:     disk,
				source:   m.Source,
				readOnly: true,
			})
			vmMount := &types.Mount{
				Type:    "erofs",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  mountPoint,
				Options: filterOptions(m.Options),
			}
			am = append(am, vmMount)
			active = append(active, activeMount{
				Source:     m.Source,
				MountPoint: mountPoint,
			})
			disks++
		case "ext4":
			disk := fmt.Sprintf("disk-%c-%s", disks, id)
			// virtiofs implementation has a limit of 36 characters for the tag
			if len(disk) > 36 {
				disk = disk[:36]
			}
			// TODO: Check read only option
			addDisks = append(addDisks, diskOptions{
				name:     disk,
				source:   m.Source,
				readOnly: false,
			})
			vmMount := &types.Mount{
				Type:    "ext4",
				Source:  fmt.Sprintf("/dev/vd%c", disks),
				Target:  mountPoint,
				Options: filterOptions(m.Options),
			}
			am = append(am, vmMount)
			active = append(active, activeMount{
				Source:     m.Source,
				MountPoint: mountPoint,
			})
			disks++
		case "overlay":
			var (
				wdi = -1
				udi = -1
			)
			for j, opt := range m.Options {
				if strings.HasPrefix(opt, "upperdir=") {
					udi = j
				} else if strings.HasPrefix(opt, "workdir=") {
					wdi = j
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
					return nil, fmt.Errorf("cannot use virtiofs for upper dir in overlay: %w", errdefs.ErrNotImplemented)
				}
			} else {
				log.G(ctx).WithField("options", m.Options).Warnf("overlayfs missing workdir or upperdir")
			}

			// Set the target for the overlay mount
			resultMount := &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  mountPoint,
				Options: m.Options,
			}
			am = append(am, resultMount)
			active = append(active, activeMount{
				Source:     m.Source,
				MountPoint: mountPoint,
			})
		case "bind":
			// Pass bind mounts through to the VM for processing
			resultMount := &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  mountPoint,
				Options: m.Options,
			}
			am = append(am, resultMount)
			active = append(active, activeMount{
				Source:     m.Source,
				MountPoint: mountPoint,
			})
		default:
			resultMount := &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  mountPoint,
				Options: m.Options,
			}
			am = append(am, resultMount)
			active = append(active, activeMount{
				Source:     m.Source,
				MountPoint: mountPoint,
			})
		}
	}

	if len(addDisks) > 10 {
		return nil, fmt.Errorf("exceeded maximum virtio disk count: %d > 10: %w", len(addDisks), errdefs.ErrNotImplemented)
	}

	for _, do := range addDisks {
		var opts []vm.MountOpt
		if do.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}
		if err := vmi.AddDisk(ctx, do.name, do.source, opts...); err != nil {
			return nil, err
		}
	}

	return am, err
}

// runMkfs runs mkfs for the given filesystem type on the source file.
func runMkfs(ctx context.Context, fsType, source string, options []string) error {
	// Parse mkfs options from mount options
	var mkfsArgs []string
	for _, opt := range options {
		if strings.HasPrefix(opt, "X-containerd.mkfs.") {
			switch {
			case strings.HasPrefix(opt, "X-containerd.mkfs.size="):
				// Size is used to create the file, not passed to mkfs
				sizeStr := strings.TrimPrefix(opt, "X-containerd.mkfs.size=")
				size, err := strconv.ParseInt(sizeStr, 10, 64)
				if err != nil {
					return fmt.Errorf("invalid mkfs size %q: %w", sizeStr, err)
				}
				// Create the file with the specified size if it doesn't exist
				if _, err := os.Stat(source); os.IsNotExist(err) {
					f, err := os.Create(source)
					if err != nil {
						return fmt.Errorf("creating image file: %w", err)
					}
					if err := f.Truncate(size); err != nil {
						f.Close()
						return fmt.Errorf("truncating image file to size %d: %w", size, err)
					}
					f.Close()
				}
			case strings.HasPrefix(opt, "X-containerd.mkfs.fs="):
				// This specifies the filesystem type, already handled
			default:
				// Unknown mkfs option, skip
				log.G(ctx).WithField("option", opt).Warn("unknown mkfs option")
			}
		}
	}

	// Check if the file already has a filesystem
	if hasFilesystem(source) {
		log.G(ctx).WithField("source", source).Debug("source already has filesystem, skipping mkfs")
		return nil
	}

	// Run mkfs
	mkfsCmd := fmt.Sprintf("mkfs.%s", fsType)
	mkfsArgs = append(mkfsArgs, source)

	log.G(ctx).WithFields(log.Fields{
		"cmd":    mkfsCmd,
		"args":   mkfsArgs,
		"source": source,
	}).Debug("running mkfs")

	cmd := exec.CommandContext(ctx, mkfsCmd, mkfsArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.%s failed: %w, output: %s", fsType, err, string(output))
	}

	return nil
}

// hasFilesystem checks if the file already has a filesystem.
func hasFilesystem(source string) bool {
	cmd := exec.Command("blkid", source)
	err := cmd.Run()
	return err == nil
}

// formatMount expands templates in the mount's source, target, and options.
func formatMount(m *types.Mount, active []activeMount) (*types.Mount, error) {
	result := &types.Mount{
		Type:    m.Type,
		Source:  m.Source,
		Target:  m.Target,
		Options: make([]string, len(m.Options)),
	}
	copy(result.Options, m.Options)

	// Expand templates in options
	for i, opt := range result.Options {
		if expanded, err := expandTemplate(opt, active); err != nil {
			return nil, fmt.Errorf("formatting mount option %q: %w", opt, err)
		} else {
			result.Options[i] = expanded
		}
	}

	// Expand template in source
	if expanded, err := expandTemplate(result.Source, active); err != nil {
		return nil, fmt.Errorf("formatting mount source %q: %w", result.Source, err)
	} else {
		result.Source = expanded
	}

	// Expand template in target
	if expanded, err := expandTemplate(result.Target, active); err != nil {
		return nil, fmt.Errorf("formatting mount target %q: %w", result.Target, err)
	} else {
		result.Target = expanded
	}

	return result, nil
}

// mkdirMount processes mkdir options and creates directories.
func mkdirMount(m *types.Mount) (*types.Mount, error) {
	result := &types.Mount{
		Type:   m.Type,
		Source: m.Source,
		Target: m.Target,
	}

	var options []string
	for _, opt := range m.Options {
		if strings.HasPrefix(opt, "X-containerd.mkdir.") {
			prefix := "X-containerd.mkdir.path="
			if !strings.HasPrefix(opt, prefix) {
				return nil, fmt.Errorf("unknown mkdir mount option %q", opt)
			}
			parts := strings.SplitN(opt[len(prefix):], ":", 4)
			if len(parts) >= 1 {
				dir := parts[0]
				// Note: Unlike mountutil.All, we don't restrict the path here
				// because we're creating directories that will be used in the VM
				if err := os.MkdirAll(dir, 0755); err != nil {
					return nil, fmt.Errorf("mkdir %s: %w", dir, err)
				}
			}
		} else {
			options = append(options, opt)
		}
	}
	result.Options = options

	return result, nil
}

// expandTemplate expands template strings like {{ mount 0 }} using active mounts.
func expandTemplate(s string, active []activeMount) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}

	fm := template.FuncMap{
		"source": func(i int) (string, error) {
			if i < 0 || i >= len(active) {
				return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(active))
			}
			return active[i].Source, nil
		},
		"mount": func(i int) (string, error) {
			if i < 0 || i >= len(active) {
				return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(active))
			}
			return active[i].MountPoint, nil
		},
		"overlay": func(start, end int) (string, error) {
			var dirs []string
			if start > end {
				if start >= len(active) || end < 0 {
					return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(active))
				}
				for i := start; i >= end; i-- {
					dirs = append(dirs, active[i].MountPoint)
				}
			} else {
				if start < 0 || end >= len(active) {
					return "", fmt.Errorf("invalid range: %d-%d, has %d active mounts", start, end, len(active))
				}
				for i := start; i <= end; i++ {
					dirs = append(dirs, active[i].MountPoint)
				}
			}
			return strings.Join(dirs, ":"), nil
		},
	}

	t, err := template.New("").Funcs(fm).Parse(s)
	if err != nil {
		return "", err
	}

	buf := bytes.NewBuffer(nil)
	if err := t.Execute(buf, nil); err != nil {
		return "", err
	}
	return buf.String(), nil
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

type bindMounter struct {
	mounts []bindMount
}

type bindMount struct {
	tag      string
	hostSrc  string
	vmTarget string
}

func (bm *bindMounter) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	for i, m := range b.Spec.Mounts {
		if m.Type != "bind" {
			continue
		}

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

		transformed := bindMount{
			tag:      tag,
			hostSrc:  hostSrc,
			vmTarget: vmTarget,
		}

		bm.mounts = append(bm.mounts, transformed)
		b.Spec.Mounts[i].Source = specSrc
	}

	return nil
}

func (bm *bindMounter) SetupVM(ctx context.Context, vmi vm.Instance) error {
	for _, m := range bm.mounts {
		if err := vmi.AddFS(ctx, m.tag, m.hostSrc); err != nil {
			return err
		}
	}
	return nil
}

func (bm *bindMounter) InitArgs() []string {
	args := make([]string, 0, len(bm.mounts))
	for _, m := range bm.mounts {
		args = append(args, "-mount="+m.tag+":"+m.vmTarget)
	}
	return args
}
