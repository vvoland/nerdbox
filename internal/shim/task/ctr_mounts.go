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
	"slices"
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

	// originalSource is the original source path before transformation.
	originalSource string

	// disk contains the disk configuration if this mount requires a block device.
	disk *diskOptions
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
	// Starts at 'a' and increments for each disk added.
	diskLetter byte
}

// FromBundle processes the bundle's OCI spec mounts and identifies those that
// need transformation for VM execution. It modifies the spec in place to update
// mount sources to VM device paths.
func (t *ctrMountTransformer) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	t.diskLetter = 'a'

	for i := range b.Spec.Mounts {
		m := &b.Spec.Mounts[i]

		transform, err := t.processCtrMount(ctx, i, m)
		if err != nil {
			return fmt.Errorf("processing mount %q: %w", m.Destination, err)
		}
		if transform != nil {
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
// 1. Formatted with mkfs on the host (handled by the snapshotter)
// 2. Attached as a block device to the VM
// 3. Mounted inside the VM at the specified destination
//
// The mount type is transformed from "mkfs/ext4" to "format/ext4" so that
// mountutil.All() in the VM can process the format/ prefix and handle templates.
func (t *ctrMountTransformer) processMkfsMount(ctx context.Context, index int, m *specs.Mount) (*ctrMountTransform, error) {
	// Extract the filesystem type from "mkfs/<fstype>"
	fsType := strings.TrimPrefix(m.Type, "mkfs/")
	if fsType == "" {
		return nil, fmt.Errorf("invalid mkfs mount type: %s", m.Type)
	}

	originalSource := m.Source

	log.G(ctx).WithFields(log.Fields{
		"source":      originalSource,
		"destination": m.Destination,
		"fstype":      fsType,
	}).Debug("processing mkfs mount for VM")

	// Check if read-only based on options
	readOnly := slices.Contains(m.Options, "ro")

	// Create a unique disk name based on the destination
	hash := sha256.Sum256([]byte(m.Destination))
	diskName := fmt.Sprintf("ctr-%c-%x", t.diskLetter, hash[:4])
	// virtiofs implementation has a limit of 36 characters for the tag
	if len(diskName) > 36 {
		diskName = diskName[:36]
	}

	// The device path inside the VM
	vmDevicePath := fmt.Sprintf("/dev/vd%c", t.diskLetter)
	t.diskLetter++

	// Update the mount in the spec to use the VM device path.
	// Keep the "format/" prefix so mountutil.All() in the VM can handle templates
	// in subsequent mounts that reference this mount.
	m.Type = "format/" + fsType
	m.Source = vmDevicePath
	// Filter out mkfs-specific options, keep only mount options
	m.Options = filterMkfsOptions(m.Options)

	return &ctrMountTransform{
		specIndex:      index,
		originalSource: originalSource,
		disk: &diskOptions{
			name:     diskName,
			source:   originalSource,
			readOnly: readOnly,
		},
	}, nil
}

// SetupVM attaches the required block devices to the VM instance.
func (t *ctrMountTransformer) SetupVM(ctx context.Context, vmi vm.Instance) error {
	for _, transform := range t.transforms {
		if transform.disk != nil {
			var opts []vm.MountOpt
			if transform.disk.readOnly {
				opts = append(opts, vm.WithReadOnly())
			}

			log.G(ctx).WithFields(log.Fields{
				"diskName": transform.disk.name,
				"source":   transform.disk.source,
				"readOnly": transform.disk.readOnly,
			}).Debug("adding block device for container mount")

			if err := vmi.AddDisk(ctx, transform.disk.name, transform.disk.source, opts...); err != nil {
				return fmt.Errorf("adding disk %s: %w", transform.disk.name, err)
			}
		}
	}
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
