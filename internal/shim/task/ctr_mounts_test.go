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
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestCtrMountTransformer(t *testing.T) {
	testcases := []struct {
		name           string
		mounts         []specs.Mount
		wantMounts     []specs.Mount
		wantNumDisks   int
		wantDiskLetter byte
	}{
		{
			name:           "no mounts",
			mounts:         nil,
			wantMounts:     nil,
			wantNumDisks:   0,
			wantDiskLetter: 'a',
		},
		{
			name: "no mkfs mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: "/host/path", Destination: "/container/path"},
			},
			wantMounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: "/host/path", Destination: "/container/path"},
			},
			wantNumDisks:   0,
			wantDiskLetter: 'a',
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
			wantMounts: []specs.Mount{
				{
					Type:        "format/ext4",
					Source:      "/dev/vda",
					Destination: "/data",
					Options:     []string{"rw"},
				},
			},
			wantNumDisks:   1,
			wantDiskLetter: 'b',
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
			wantMounts: []specs.Mount{
				{
					Type:        "format/ext4",
					Source:      "/dev/vda",
					Destination: "/data",
					Options:     []string{"rw"},
				},
				// format/mkdir/bind mount is passed through unchanged -
				// it uses templates that mountutil.All() in the VM will process
				{
					Type:        "format/mkdir/bind",
					Source:      "{{ mount 0 }}/upper",
					Destination: "/data",
					Options:     []string{"X-containerd.mkdir.path={{ mount 0 }}/upper:0755", "rw", "rbind"},
				},
			},
			wantNumDisks:   1,
			wantDiskLetter: 'b',
		},
		{
			name: "mkfs/ext4 readonly mount",
			mounts: []specs.Mount{
				{
					Type:        "mkfs/ext4",
					Source:      "/path/to/readonly.img",
					Destination: "/readonly-data",
					Options:     []string{"ro", "loop"},
				},
			},
			wantMounts: []specs.Mount{
				{
					Type:        "format/ext4",
					Source:      "/dev/vda",
					Destination: "/readonly-data",
					Options:     []string{"ro"},
				},
			},
			wantNumDisks:   1,
			wantDiskLetter: 'b',
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
			wantMounts: []specs.Mount{
				{
					Type:        "format/ext4",
					Source:      "/dev/vda",
					Destination: "/data1",
					Options:     []string{"rw"},
				},
				{
					Type:        "format/ext4",
					Source:      "/dev/vdb",
					Destination: "/data2",
					Options:     []string{"rw"},
				},
			},
			wantNumDisks:   2,
			wantDiskLetter: 'c',
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

			// Verify that the spec mounts were transformed
			assert.Equal(t, tc.wantMounts, b.Spec.Mounts)

			// Verify the number of transforms
			assert.Equal(t, tc.wantNumDisks, len(transformer.transforms))

			// Verify disk letter progression
			assert.Equal(t, tc.wantDiskLetter, transformer.diskLetter)

			// Verify each transform has a valid disk config
			for i, transform := range transformer.transforms {
				assert.NotNil(t, transform.disk, "transform %d should have disk", i)
				// Verify disk name format: ctr-<letter>-<hash>
				assert.True(t, strings.HasPrefix(transform.disk.name, "ctr-"), "disk name should start with 'ctr-'")
				assert.LessOrEqual(t, len(transform.disk.name), 36, "disk name should be <= 36 chars")
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
	assert.Equal(t, originalSource, transformer.transforms[0].disk.source)

	// But the spec mount should have been updated to the VM device path
	assert.Equal(t, "/dev/vda", b.Spec.Mounts[0].Source)
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
	assert.True(t, transformer.transforms[0].disk.readOnly)
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
