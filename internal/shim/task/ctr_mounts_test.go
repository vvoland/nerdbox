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
	"net"
	"testing"

	"github.com/containerd/ttrpc"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

func TestCtrMountTransformerFromBundle(t *testing.T) {
	testcases := []struct {
		name              string
		mounts            []specs.Mount
		wantNumTransforms int
		// After FromBundle, mkfs options should be filtered but type/source unchanged
		wantOptions [][]string
	}{
		{
			name:              "no mounts",
			mounts:            nil,
			wantNumTransforms: 0,
		},
		{
			name: "no mkfs mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: "/host/path", Destination: "/container/path"},
			},
			wantNumTransforms: 0,
		},
		{
			name: "single mkfs/ext4 mount",
			mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/rwlayer.img",
					Destination: "/data",
					Options:     []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw", "loop"},
				},
			},
			wantNumTransforms: 1,
			wantOptions:       [][]string{{"rw"}},
		},
		{
			name: "mkfs/ext4 with format/mkdir/bind (typical erofs+rw pattern)",
			mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/rwlayer.img",
					Destination: "/data",
					Options:     []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw", "loop"},
				},
				{
					Type:        "format/mkdir/bind",
					Source:      "{{ mount 0 }}/upper",
					Destination: "/data",
					Options:     []string{"X-containerd.mkdir.path={{ mount 0 }}/upper:0755", "rw", "rbind"},
				},
			},
			wantNumTransforms: 1, // Only the mkfs mount creates a transform
			wantOptions:       [][]string{{"rw"}},
		},
		{
			name: "multiple mkfs mounts",
			mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/data1.img",
					Destination: "/data1",
					Options:     []string{"rw", "loop"},
				},
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/data2.img",
					Destination: "/data2",
					Options:     []string{"rw", "loop"},
				},
			},
			wantNumTransforms: 2,
			wantOptions:       [][]string{{"rw"}, {"rw"}},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{
				Spec: specs.Spec{
					Mounts: tc.mounts,
				},
			}

			transformer := &ctrMountTransformer{}
			err := transformer.FromBundle(context.Background(), b)
			require.NoError(t, err)

			// Verify the number of transforms
			assert.Equal(t, tc.wantNumTransforms, len(transformer.transforms))

			// Verify options were filtered
			for i, transform := range transformer.transforms {
				if tc.wantOptions != nil && i < len(tc.wantOptions) {
					assert.Equal(t, tc.wantOptions[i], transform.specMount.Options,
						"transform %d options mismatch", i)
				}
			}
		})
	}
}

func TestCtrMountTransformerPreservesOriginalSource(t *testing.T) {
	originalSource := "/path/to/rwlayer.img"
	b := &bundle.Bundle{
		Spec: specs.Spec{
			Mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      originalSource,
					Destination: "/data",
					Options:     []string{"rw", "loop"},
				},
			},
		},
	}

	transformer := &ctrMountTransformer{}
	err := transformer.FromBundle(context.Background(), b)
	require.NoError(t, err)

	require.Len(t, transformer.transforms, 1)
	// The transform should preserve the original source for SetupVM
	assert.Equal(t, originalSource, transformer.transforms[0].originalSource)
	// The spec mount source should still be the original (until SetupVM is called)
	assert.Equal(t, originalSource, b.Spec.Mounts[0].Source)
}

func TestCtrMountTransformerReadOnly(t *testing.T) {
	b := &bundle.Bundle{
		Spec: specs.Spec{
			Mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/readonly.img",
					Destination: "/data",
					Options:     []string{"ro"},
				},
			},
		},
	}

	transformer := &ctrMountTransformer{}
	err := transformer.FromBundle(context.Background(), b)
	require.NoError(t, err)

	require.Len(t, transformer.transforms, 1)
	assert.True(t, transformer.transforms[0].readOnly)
}

func TestCtrMountTransformerDiskLetterProgression(t *testing.T) {
	b := &bundle.Bundle{
		Spec: specs.Spec{
			Mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/data1.img",
					Destination: "/data1",
					Options:     []string{"rw"},
				},
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/data2.img",
					Destination: "/data2",
					Options:     []string{"rw"},
				},
			},
		},
	}

	transformer := &ctrMountTransformer{}
	err := transformer.FromBundle(context.Background(), b)
	require.NoError(t, err)

	// Simulate that rootfs mounts used 'a' and 'b', so we start from 'c'
	transformer.SetStartingDiskLetter('c')

	// Create a mock VM instance that just tracks AddDisk calls
	mockVM := &mockVMInstance{}
	err = transformer.SetupVM(context.Background(), mockVM)
	require.NoError(t, err)

	// Verify that disks were added
	assert.Equal(t, 2, len(mockVM.disks))

	// Verify the spec mounts were updated with correct device paths
	assert.Equal(t, "format/ext4", b.Spec.Mounts[0].Type)
	assert.Equal(t, "/dev/vdc", b.Spec.Mounts[0].Source)
	assert.Equal(t, "format/ext4", b.Spec.Mounts[1].Type)
	assert.Equal(t, "/dev/vdd", b.Spec.Mounts[1].Source)
}

func TestFilterMkfsOptions(t *testing.T) {
	testcases := []struct {
		name    string
		options []string
		want    []string
	}{
		{
			name:    "nil options",
			options: nil,
			want:    nil,
		},
		{
			name:    "empty options",
			options: []string{},
			want:    nil,
		},
		{
			name:    "only standard options",
			options: []string{"rw", "noatime"},
			want:    []string{"rw", "noatime"},
		},
		{
			name:    "filter mkfs options",
			options: []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw"},
			want:    []string{"rw"},
		},
		{
			name:    "filter loop option",
			options: []string{"rw", "loop"},
			want:    []string{"rw"},
		},
		{
			name:    "filter all special options",
			options: []string{"X-containerd.mkfs.fs=ext4", "X-containerd.mkfs.size=67108864", "rw", "loop", "noatime"},
			want:    []string{"rw", "noatime"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterMkfsOptions(tc.options)
			assert.Equal(t, tc.want, got)
		})
	}
}

// mockVMInstance is a test double for vm.Instance
type mockVMInstance struct {
	disks []mockDisk
}

type mockDisk struct {
	name     string
	source   string
	readOnly bool
}

func (m *mockVMInstance) AddDisk(ctx context.Context, name, source string, opts ...vm.MountOpt) error {
	disk := mockDisk{name: name, source: source}
	cfg := &vm.MountConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	disk.readOnly = cfg.Readonly
	m.disks = append(m.disks, disk)
	return nil
}

func (m *mockVMInstance) AddFS(ctx context.Context, tag, path string, opts ...vm.MountOpt) error {
	return nil
}

func (m *mockVMInstance) AddNIC(ctx context.Context, endpoint string, mac net.HardwareAddr, mode vm.NetworkMode, features, flags uint32) error {
	return nil
}

func (m *mockVMInstance) Start(ctx context.Context, opts ...vm.StartOpt) error {
	return nil
}

func (m *mockVMInstance) Client() *ttrpc.Client {
	return nil
}

func (m *mockVMInstance) Shutdown(ctx context.Context) error {
	return nil
}

func (m *mockVMInstance) StartStream(ctx context.Context) (uint32, net.Conn, error) {
	return 0, nil, nil
}
