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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
)

func TestBindMountsProvider(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a test file
	testfile := filepath.Join(tmpDir, "testfile.txt")
	f, err := os.Create(testfile)
	assert.NoError(t, err)
	f.Close()

	// Create a test directory
	testdirData := filepath.Join(tmpDir, "testdir", "data")
	assert.NoError(t, os.MkdirAll(testdirData, 0755))
	testdirConfig := filepath.Join(tmpDir, "testdir", "config")
	assert.NoError(t, os.MkdirAll(testdirConfig, 0755))

	testcases := []struct {
		name            string
		mounts          []specs.Mount
		wantMounts      []specMount
		wantSpecSources []string // expected sources in the OCI spec after transformation
		wantSpecTypes   []string // expected types in the OCI spec after transformation
		wantInitArgs    []string
	}{
		{
			name:            "no mounts",
			mounts:          nil,
			wantMounts:      nil,
			wantSpecSources: nil,
			wantSpecTypes:   nil,
			wantInitArgs:    []string{},
		},
		{
			name: "no bind mounts",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantMounts:      nil,
			wantSpecSources: []string{"tmpfs", "proc"},
			wantSpecTypes:   []string{"tmpfs", "proc"},
			wantInitArgs:    []string{},
		},
		{
			name: "single bind mount",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
			},
			wantMounts: []specMount{
				{
					mountType: "bind",
					tag:       "bind-8c5eaa445dd84f17",
					hostSrc:   testdirData,
					vmTarget:  "/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{"/mnt/bind-8c5eaa445dd84f17"},
			wantSpecTypes:   []string{"bind"},
			wantInitArgs:    []string{"-mount=virtiofs:bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17"},
		},
		{
			name: "multiple bind mounts",
			mounts: []specs.Mount{
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
				{Type: "bind", Source: testdirConfig, Destination: "/container/config"},
			},
			wantMounts: []specMount{
				{
					mountType: "bind",
					tag:       "bind-8c5eaa445dd84f17",
					hostSrc:   testdirData,
					vmTarget:  "/mnt/bind-8c5eaa445dd84f17",
				},
				{
					mountType: "bind",
					tag:       "bind-529984c9ac58b7ec",
					hostSrc:   testdirConfig,
					vmTarget:  "/mnt/bind-529984c9ac58b7ec",
				},
			},
			wantSpecSources: []string{
				"/mnt/bind-8c5eaa445dd84f17",
				"/mnt/bind-529984c9ac58b7ec",
			},
			wantSpecTypes: []string{"bind", "bind"},
			wantInitArgs: []string{
				"-mount=virtiofs:bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17",
				"-mount=virtiofs:bind-529984c9ac58b7ec:/mnt/bind-529984c9ac58b7ec",
			},
		},
		{
			name: "mixed mount types",
			mounts: []specs.Mount{
				{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
				{Type: "bind", Source: testdirData, Destination: "/container/data"},
				{Type: "proc", Source: "proc", Destination: "/proc"},
			},
			wantMounts: []specMount{
				{
					mountType: "bind",
					tag:       "bind-8c5eaa445dd84f17",
					hostSrc:   testdirData,
					vmTarget:  "/mnt/bind-8c5eaa445dd84f17",
				},
			},
			wantSpecSources: []string{
				"tmpfs",
				"/mnt/bind-8c5eaa445dd84f17",
				"proc",
			},
			wantSpecTypes: []string{"tmpfs", "bind", "proc"},
			wantInitArgs:  []string{"-mount=virtiofs:bind-8c5eaa445dd84f17:/mnt/bind-8c5eaa445dd84f17"},
		},
		{
			name: "single file bind mount",
			mounts: []specs.Mount{
				{Type: "bind", Source: testfile, Destination: "/container/testfile"},
			},
			wantMounts: []specMount{
				{
					mountType: "bind",
					tag:       "bind-6dace5108a719565",
					hostSrc:   tmpDir,
					vmTarget:  "/mnt/bind-6dace5108a719565",
				},
			},
			wantSpecSources: []string{"/mnt/bind-6dace5108a719565/testfile.txt"},
			wantSpecTypes:   []string{"bind"},
			wantInitArgs:    []string{"-mount=virtiofs:bind-6dace5108a719565:/mnt/bind-6dace5108a719565"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle.Bundle{
				Spec: specs.Spec{
					Mounts: tc.mounts,
				},
			}

			bm := &bindMounter{}
			err := bm.FromBundle(context.Background(), b)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantMounts, bm.mounts)

			// Verify that the spec sources were transformed
			for i, wantSource := range tc.wantSpecSources {
				assert.Equal(t, wantSource, b.Spec.Mounts[i].Source)
			}

			// Verify that the spec types were transformed
			for i, wantType := range tc.wantSpecTypes {
				assert.Equal(t, wantType, b.Spec.Mounts[i].Type)
			}

			// Verify the args passed to vminitd
			args := bm.InitArgs()
			assert.Equal(t, tc.wantInitArgs, args)
		})
	}
}

func TestErofsMountsProvider(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test erofs image files
	erofsImage := filepath.Join(tmpDir, "rootfs.erofs")
	f, err := os.Create(erofsImage)
	require.NoError(t, err)
	f.Close()

	erofsImage2 := filepath.Join(tmpDir, "data.erofs")
	f2, err := os.Create(erofsImage2)
	require.NoError(t, err)
	f2.Close()

	t.Run("single erofs mount", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "erofs", Source: erofsImage, Destination: "/data"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 1)
		m := bm.mounts[0]
		assert.Equal(t, "erofs", m.mountType)
		assert.True(t, m.isBlockDevice)
		assert.True(t, strings.HasPrefix(m.tag, "blk-"))
		assert.Equal(t, erofsImage, m.hostSrc)
		assert.True(t, strings.HasPrefix(m.vmTarget, "/mnt/blk-"))
		assert.Equal(t, "/dev/vda", m.vmSource)
		assert.False(t, m.readOnly) // read-only is determined by options, not filesystem type

		// Verify spec transformation
		assert.Equal(t, "bind", b.Spec.Mounts[0].Type)
		assert.True(t, strings.HasPrefix(b.Spec.Mounts[0].Source, "/mnt/blk-"))
		assert.Contains(t, b.Spec.Mounts[0].Options, "bind")

		// Verify init args
		args := bm.InitArgs()
		require.Len(t, args, 1)
		assert.True(t, strings.HasPrefix(args[0], "-mount=erofs:/dev/vda:/mnt/blk-"))
	})

	t.Run("multiple erofs mounts", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "erofs", Source: erofsImage, Destination: "/data"},
					{Type: "erofs", Source: erofsImage2, Destination: "/data2"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 2)

		// First mount should get /dev/vda
		assert.Equal(t, "/dev/vda", bm.mounts[0].vmSource)
		// Second mount should get /dev/vdb
		assert.Equal(t, "/dev/vdb", bm.mounts[1].vmSource)

		// Verify init args
		args := bm.InitArgs()
		require.Len(t, args, 2)
		assert.Contains(t, args[0], "erofs:/dev/vda:")
		assert.Contains(t, args[1], "erofs:/dev/vdb:")
	})

	t.Run("erofs mount with options", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "erofs", Source: erofsImage, Destination: "/data", Options: []string{"ro", "noatime"}},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 1)
		assert.Equal(t, []string{"ro", "noatime"}, bm.mounts[0].options)

		// Verify spec transformation - only bind-compatible options should remain
		assert.Contains(t, b.Spec.Mounts[0].Options, "bind")
		assert.Contains(t, b.Spec.Mounts[0].Options, "ro")
		assert.NotContains(t, b.Spec.Mounts[0].Options, "noatime") // noatime is not a bind option

		// Verify init args include options
		args := bm.InitArgs()
		require.Len(t, args, 1)
		assert.Contains(t, args[0], ":ro,noatime")
	})
}

func TestExt4MountsProvider(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test ext4 image files
	ext4Image := filepath.Join(tmpDir, "data.ext4")
	f, err := os.Create(ext4Image)
	require.NoError(t, err)
	f.Close()

	t.Run("single ext4 mount", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "ext4", Source: ext4Image, Destination: "/data"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 1)
		m := bm.mounts[0]
		assert.Equal(t, "ext4", m.mountType)
		assert.True(t, m.isBlockDevice)
		assert.True(t, strings.HasPrefix(m.tag, "blk-"))
		assert.Equal(t, ext4Image, m.hostSrc)
		assert.True(t, strings.HasPrefix(m.vmTarget, "/mnt/blk-"))
		assert.Equal(t, "/dev/vda", m.vmSource)
		assert.False(t, m.readOnly) // ext4 is read-write by default

		// Verify spec transformation
		assert.Equal(t, "bind", b.Spec.Mounts[0].Type)
		assert.True(t, strings.HasPrefix(b.Spec.Mounts[0].Source, "/mnt/blk-"))

		// Verify init args
		args := bm.InitArgs()
		require.Len(t, args, 1)
		assert.True(t, strings.HasPrefix(args[0], "-mount=ext4:/dev/vda:/mnt/blk-"))
	})

	t.Run("ext4 mount with ro option", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "ext4", Source: ext4Image, Destination: "/data", Options: []string{"ro"}},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 1)
		assert.True(t, bm.mounts[0].readOnly)

		// Verify init args include ro option
		args := bm.InitArgs()
		require.Len(t, args, 1)
		assert.Contains(t, args[0], ":ro")
	})
}

func TestMixedMountsProvider(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test files
	testdir := filepath.Join(tmpDir, "testdir")
	require.NoError(t, os.MkdirAll(testdir, 0755))

	erofsImage := filepath.Join(tmpDir, "rootfs.erofs")
	f, err := os.Create(erofsImage)
	require.NoError(t, err)
	f.Close()

	ext4Image := filepath.Join(tmpDir, "data.ext4")
	f2, err := os.Create(ext4Image)
	require.NoError(t, err)
	f2.Close()

	t.Run("mixed bind, erofs, and ext4 mounts", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "bind", Source: testdir, Destination: "/container/bind"},
					{Type: "erofs", Source: erofsImage, Destination: "/container/erofs"},
					{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
					{Type: "ext4", Source: ext4Image, Destination: "/container/ext4"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		// Should have 3 mounts: bind, erofs, and ext4
		assert.Len(t, bm.mounts, 3)

		// Verify mount types
		assert.Equal(t, "bind", bm.mounts[0].mountType)
		assert.Equal(t, "erofs", bm.mounts[1].mountType)
		assert.Equal(t, "ext4", bm.mounts[2].mountType)

		// Verify disk letters are assigned correctly (erofs gets 'a', ext4 gets 'b')
		assert.Equal(t, "/dev/vda", bm.mounts[1].vmSource)
		assert.Equal(t, "/dev/vdb", bm.mounts[2].vmSource)

		// Verify spec transformations
		assert.Equal(t, "bind", b.Spec.Mounts[0].Type)
		assert.Equal(t, "bind", b.Spec.Mounts[1].Type) // erofs transformed to bind
		assert.Equal(t, "tmpfs", b.Spec.Mounts[2].Type)
		assert.Equal(t, "bind", b.Spec.Mounts[3].Type) // ext4 transformed to bind

		// Verify init args
		args := bm.InitArgs()
		assert.Len(t, args, 3)
		assert.Contains(t, args[0], "virtiofs:")
		assert.Contains(t, args[1], "erofs:/dev/vda:")
		assert.Contains(t, args[2], "ext4:/dev/vdb:")
	})
}

func TestDiskOffset(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test erofs image files
	erofsImage := filepath.Join(tmpDir, "data.erofs")
	f, err := os.Create(erofsImage)
	require.NoError(t, err)
	f.Close()

	ext4Image := filepath.Join(tmpDir, "data.ext4")
	f2, err := os.Create(ext4Image)
	require.NoError(t, err)
	f2.Close()

	t.Run("disk offset from rootfs", func(t *testing.T) {
		// Simulate that rootfs used 3 disks (vda, vdb, vdc)
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "erofs", Source: erofsImage, Destination: "/data1"},
					{Type: "ext4", Source: ext4Image, Destination: "/data2"},
				},
			},
		}

		bm := &bindMounter{}
		// Set offset to simulate 3 rootfs disks already used
		bm.SetDiskOffset(3)

		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		require.Len(t, bm.mounts, 2)

		// Disks should start from 'd' (after a, b, c)
		assert.Equal(t, "/dev/vdd", bm.mounts[0].vmSource)
		assert.Equal(t, "/dev/vde", bm.mounts[1].vmSource)

		// Verify init args have correct device paths
		args := bm.InitArgs()
		require.Len(t, args, 2)
		assert.Contains(t, args[0], "erofs:/dev/vdd:")
		assert.Contains(t, args[1], "ext4:/dev/vde:")
	})

	t.Run("disk count", func(t *testing.T) {
		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "erofs", Source: erofsImage, Destination: "/data1"},
					{Type: "ext4", Source: ext4Image, Destination: "/data2"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		// Should report 2 disks used
		assert.Equal(t, byte(2), bm.DiskCount())
	})

	t.Run("disk count with bind mounts", func(t *testing.T) {
		testdir := filepath.Join(tmpDir, "testdir")
		require.NoError(t, os.MkdirAll(testdir, 0755))

		b := &bundle.Bundle{
			Spec: specs.Spec{
				Mounts: []specs.Mount{
					{Type: "bind", Source: testdir, Destination: "/bind"},
					{Type: "erofs", Source: erofsImage, Destination: "/data1"},
				},
			},
		}

		bm := &bindMounter{}
		err := bm.FromBundle(context.Background(), b)
		require.NoError(t, err)

		// Should report 1 disk (bind mounts don't use disks)
		assert.Equal(t, byte(1), bm.DiskCount())
	})
}

func TestFilterSpecOptions(t *testing.T) {
	testcases := []struct {
		name    string
		options []string
		want    []string
	}{
		{
			name:    "empty options",
			options: nil,
			want:    nil,
		},
		{
			name:    "filter loop",
			options: []string{"loop", "ro"},
			want:    []string{"ro"},
		},
		{
			name:    "filter bind options",
			options: []string{"bind", "rbind", "ro", "private"},
			want:    []string{"ro"},
		},
		{
			name:    "keep filesystem options",
			options: []string{"ro", "noatime", "nodiratime"},
			want:    []string{"ro", "noatime", "nodiratime"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterSpecOptions(tc.options)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFilterBindOptions(t *testing.T) {
	testcases := []struct {
		name    string
		options []string
		want    []string
	}{
		{
			name:    "empty options - adds bind",
			options: nil,
			want:    []string{"bind"},
		},
		{
			name:    "only bind options",
			options: []string{"ro", "private"},
			want:    []string{"bind", "ro", "private"},
		},
		{
			name:    "has bind already",
			options: []string{"bind", "ro"},
			want:    []string{"bind", "ro"},
		},
		{
			name:    "has rbind already",
			options: []string{"rbind", "ro"},
			want:    []string{"rbind", "ro"},
		},
		{
			name:    "filters non-bind options",
			options: []string{"ro", "noatime", "nodiratime"},
			want:    []string{"bind", "ro"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterBindOptions(tc.options)
			assert.Equal(t, tc.want, got)
		})
	}
}
