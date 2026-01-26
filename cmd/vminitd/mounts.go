//go:build linux

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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
)

// vmMount represents a mount to be set up in the VM.
// It supports different mount types:
//   - virtiofs: for bind mounts from host (shared via virtio-fs)
//   - erofs: for read-only EROFS filesystem images (passed as virtio block devices)
//   - ext4: for ext4 filesystem images (passed as virtio block devices)
type vmMount struct {
	// mountType is the filesystem type (virtiofs, erofs, ext4)
	mountType string
	// source is the mount source:
	//   - for virtiofs: the virtio-fs tag
	//   - for erofs/ext4: the block device path (e.g., /dev/vda)
	source string
	// target is the mount target path in the VM
	target string
	// options are mount options
	options []string
}

type vmMounts []vmMount

func (m *vmMounts) String() string {
	ss := make([]string, 0, len(*m))
	for _, vm := range *m {
		ss = append(ss, vm.String())
	}
	return strings.Join(ss, ",")
}

func (vm *vmMount) String() string {
	s := vm.mountType + ":" + vm.source + ":" + vm.target
	if len(vm.options) > 0 {
		s += ":" + strings.Join(vm.options, ",")
	}
	return s
}

func (m *vmMounts) Set(value string) error {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) < 3 {
		return fmt.Errorf("invalid mount %q: expected format: type:source:target[:options]", value)
	}

	mountType := parts[0]
	source := parts[1]
	target := parts[2]

	if len(mountType) == 0 || len(source) == 0 || len(target) == 0 {
		return fmt.Errorf("invalid mount %q: type, source, and target cannot be empty", value)
	}

	// Validate mount type
	switch mountType {
	case "virtiofs", "erofs", "ext4":
		// valid types
	default:
		return fmt.Errorf("invalid mount type %q: supported types are virtiofs, erofs, ext4", mountType)
	}

	vm := vmMount{
		mountType: mountType,
		source:    source,
		target:    target,
	}

	if len(parts) == 4 && len(parts[3]) > 0 {
		vm.options = strings.Split(parts[3], ",")
	}

	*m = append(*m, vm)
	return nil
}

func (m *vmMounts) mountAll(ctx context.Context) error {
	for _, vm := range *m {
		log.G(ctx).WithFields(log.Fields{
			"type":    vm.mountType,
			"source":  vm.source,
			"target":  vm.target,
			"options": vm.options,
		}).Info("mounting filesystem")

		if err := os.MkdirAll(vm.target, 0700); err != nil {
			return fmt.Errorf("failed to create mount target directory %s: %w", vm.target, err)
		}

		mnt := mount.Mount{
			Type:    vm.mountType,
			Source:  vm.source,
			Target:  vm.target,
			Options: vm.options,
		}

		if err := mount.All([]mount.Mount{mnt}, "/"); err != nil {
			return fmt.Errorf("failed to mount %s at %s: %w", vm.source, vm.target, err)
		}
	}
	return nil
}
