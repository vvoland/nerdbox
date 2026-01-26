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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

// ctrMountTransform describes a container mount that needs VM-side transformation.
// These are mounts defined in the OCI spec (not rootfs mounts) that require
// block devices to be attached to the VM.
type ctrMountTransform struct {
	// specIndex is the index of this mount in the OCI spec's Mounts slice.
	specIndex int

	// specMount is a pointer to the mount in the spec that will be updated
	// with the VM device path once the disk letter is known.
	specMount *specs.Mount

	// originalSource is the original source path before transformation.
	originalSource string

	// fsType is the filesystem type (e.g., "ext4").
	fsType string

	// size is the size of the disk image to create (from X-containerd.mkfs.size option).
	// If 0, the file must already exist.
	size int64

	// readOnly indicates if the disk should be read-only.
	readOnly bool
}

// ctrMountTransformer transforms container mounts (from OCI spec) that require
// block devices to be attached to the VM. This handles mounts with types like
// "mkfs/ext4" which need the image file to be formatted and mounted as a block
// device inside the VM.
//
// It also handles the companion "format/mkdir/bind" mounts that reference the
// block device mounts using templates like "{{ mount 0 }}/upper".
type ctrMountTransformer struct {
	transforms []ctrMountTransform

	// diskLetter tracks the next available disk letter for virtio block devices.
	diskLetter byte
}

// SetStartingDiskLetter sets the starting disk letter for new block devices.
// This should be called with the next available letter after rootfs mounts
// have been set up.
func (t *ctrMountTransformer) SetStartingDiskLetter(letter byte) {
	t.diskLetter = letter
}

// FromBundle processes the bundle's OCI spec mounts and identifies those that
// need transformation for VM execution. It collects information but does NOT
// modify the spec yet - that happens in SetupVM when we know the disk letters.
func (t *ctrMountTransformer) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	log.G(ctx).WithField("numMounts", len(b.Spec.Mounts)).Debug("ctrMountTransformer.FromBundle processing spec mounts")

	for i := range b.Spec.Mounts {
		m := &b.Spec.Mounts[i]

		log.G(ctx).WithFields(log.Fields{
			"index":       i,
			"type":        m.Type,
			"source":      m.Source,
			"destination": m.Destination,
		}).Debug("examining spec mount")

		transform, err := t.processCtrMount(ctx, i, m)
		if err != nil {
			return fmt.Errorf("processing mount %q: %w", m.Destination, err)
		}
		if transform != nil {
			log.G(ctx).WithFields(log.Fields{
				"index":          i,
				"destination":    m.Destination,
				"originalSource": transform.originalSource,
				"fsType":         transform.fsType,
			}).Debug("mount requires transformation")
			t.transforms = append(t.transforms, *transform)
		}
	}

	return nil
}

// processCtrMount examines a single mount and returns a transform if the mount
// requires VM-side changes (like attaching a block device).
func (t *ctrMountTransformer) processCtrMount(ctx context.Context, index int, m *specs.Mount) (*ctrMountTransform, error) {
	// Handle mkfs/<fstype> type mounts - these are block device images that need
	// to be formatted (if not already) and mounted inside the VM.
	if strings.HasPrefix(m.Type, "mkfs/") {
		return t.processMkfsMount(ctx, index, m)
	}

	return nil, nil
}

// processMkfsMount handles mounts with type "mkfs/<fstype>" (e.g., "mkfs/ext4").
// These mounts have a source pointing to an image file that should be:
// 1. Created and formatted with mkfs (if X-containerd.mkfs.size is specified)
// 2. Attached as a block device to the VM
// 3. Mounted inside the VM at the specified destination
func (t *ctrMountTransformer) processMkfsMount(ctx context.Context, index int, m *specs.Mount) (*ctrMountTransform, error) {
	// Extract the filesystem type from "mkfs/<fstype>"
	fsType := strings.TrimPrefix(m.Type, "mkfs/")
	if fsType == "" {
		return nil, fmt.Errorf("invalid mkfs mount type: %s", m.Type)
	}

	originalSource := m.Source

	// Extract size from options (X-containerd.mkfs.size=<bytes>)
	var size int64
	for _, o := range m.Options {
		if strings.HasPrefix(o, "X-containerd.mkfs.size=") {
			sizeStr := strings.TrimPrefix(o, "X-containerd.mkfs.size=")
			var err error
			size, err = strconv.ParseInt(sizeStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid mkfs size %q: %w", sizeStr, err)
			}
			break
		}
	}

	log.G(ctx).WithFields(log.Fields{
		"source":      originalSource,
		"destination": m.Destination,
		"fstype":      fsType,
		"size":        size,
	}).Debug("processing mkfs mount for VM")

	// Check if read-only based on options
	readOnly := slices.Contains(m.Options, "ro")

	// Filter out mkfs-specific options now, keep only mount options
	m.Options = filterMkfsOptions(m.Options)

	return &ctrMountTransform{
		specIndex:      index,
		specMount:      m,
		originalSource: originalSource,
		fsType:         fsType,
		size:           size,
		readOnly:       readOnly,
	}, nil
}

// SetupVM attaches the required block devices to the VM instance and updates
// the spec mounts with the VM device paths. For mkfs mounts, it also creates
// and formats the disk image file if it doesn't exist.
func (t *ctrMountTransformer) SetupVM(ctx context.Context, vmi vm.Instance) error {
	// If diskLetter wasn't set, start from 'a' (though this shouldn't happen
	// in normal usage since SetStartingDiskLetter should be called first)
	if t.diskLetter == 0 {
		t.diskLetter = 'a'
	}

	log.G(ctx).WithFields(log.Fields{
		"startingDiskLetter": string(t.diskLetter),
		"numTransforms":      len(t.transforms),
	}).Debug("ctrMountTransformer.SetupVM starting")

	for i, transform := range t.transforms {
		// Create a unique disk name based on the destination
		hash := sha256.Sum256([]byte(transform.specMount.Destination))
		diskName := fmt.Sprintf("ctr-%c-%x", t.diskLetter, hash[:4])
		// virtiofs implementation has a limit of 36 characters for the tag
		if len(diskName) > 36 {
			diskName = diskName[:36]
		}

		// The device path inside the VM
		vmDevicePath := fmt.Sprintf("/dev/vd%c", t.diskLetter)

		// Check if source file exists, create it if needed
		if _, err := os.Stat(transform.originalSource); os.IsNotExist(err) {
			if transform.size <= 0 {
				return fmt.Errorf("source file %q does not exist and no size specified to create it", transform.originalSource)
			}

			log.G(ctx).WithFields(log.Fields{
				"source": transform.originalSource,
				"size":   transform.size,
				"fsType": transform.fsType,
			}).Debug("creating and formatting disk image")

			if err := createAndFormatDisk(ctx, transform.originalSource, transform.size, transform.fsType); err != nil {
				return fmt.Errorf("creating disk image %s: %w", transform.originalSource, err)
			}
		} else if err != nil {
			return fmt.Errorf("checking source file %q: %w", transform.originalSource, err)
		} else {
			fi, _ := os.Stat(transform.originalSource)
			log.G(ctx).WithFields(log.Fields{
				"source": transform.originalSource,
				"size":   fi.Size(),
				"mode":   fi.Mode(),
			}).Debug("source file exists")
		}

		log.G(ctx).WithFields(log.Fields{
			"index":        i,
			"diskName":     diskName,
			"source":       transform.originalSource,
			"vmDevicePath": vmDevicePath,
			"readOnly":     transform.readOnly,
			"destination":  transform.specMount.Destination,
		}).Debug("adding block device for container mount")

		var opts []vm.MountOpt
		if transform.readOnly {
			opts = append(opts, vm.WithReadOnly())
		}

		if err := vmi.AddDisk(ctx, diskName, transform.originalSource, opts...); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{
				"diskName":    diskName,
				"source":      transform.originalSource,
				"destination": transform.specMount.Destination,
			}).Error("failed to add disk to VM")
			return fmt.Errorf("adding disk %s (source=%s, dest=%s): %w", diskName, transform.originalSource, transform.specMount.Destination, err)
		}

		// Now update the mount in the spec to use the VM device path.
		// Keep the "format/" prefix so the VM-side code can handle templates
		// in subsequent mounts that reference this mount.
		transform.specMount.Type = "format/" + transform.fsType
		transform.specMount.Source = vmDevicePath

		t.diskLetter++
	}
	return nil
}

// createAndFormatDisk creates a sparse file of the specified size and formats it
// with the specified filesystem type.
func createAndFormatDisk(ctx context.Context, path string, size int64, fsType string) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Create the sparse file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}

	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("truncating file to %d bytes: %w", size, err)
	}

	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("closing file: %w", err)
	}

	// Format the file with mkfs
	mkfsCmd := fmt.Sprintf("mkfs.%s", fsType)
	cmd := exec.CommandContext(ctx, mkfsCmd, "-F", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("running %s: %w, output: %s", mkfsCmd, err, string(output))
	}

	log.G(ctx).WithFields(log.Fields{
		"path":   path,
		"size":   size,
		"fsType": fsType,
	}).Debug("disk image created and formatted")

	return nil
}

// filterMkfsOptions removes mkfs-specific options from mount options,
// keeping only standard mount options.
func filterMkfsOptions(options []string) []string {
	var filtered []string
	for _, o := range options {
		// Skip X-containerd.mkfs.* options - these are for the snapshotter
		if strings.HasPrefix(o, "X-containerd.mkfs.") {
			continue
		}
		// Skip "loop" option - not needed when using virtio block device
		if o == "loop" {
			continue
		}
		filtered = append(filtered, o)
	}
	return filtered
}
