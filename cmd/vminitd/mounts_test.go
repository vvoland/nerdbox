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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseMounts(t *testing.T) {
	testcases := []struct {
		name    string
		inputs  []string
		want    vmMounts
		wantStr string
	}{
		{
			name:   "single virtiofs mount",
			inputs: []string{"virtiofs:foo:/mnt/foo"},
			want: vmMounts{
				{mountType: "virtiofs", source: "foo", target: "/mnt/foo"},
			},
			wantStr: "virtiofs:foo:/mnt/foo",
		},
		{
			name:   "single erofs mount",
			inputs: []string{"erofs:/dev/vda:/mnt/rootfs"},
			want: vmMounts{
				{mountType: "erofs", source: "/dev/vda", target: "/mnt/rootfs"},
			},
			wantStr: "erofs:/dev/vda:/mnt/rootfs",
		},
		{
			name:   "single ext4 mount",
			inputs: []string{"ext4:/dev/vdb:/mnt/data"},
			want: vmMounts{
				{mountType: "ext4", source: "/dev/vdb", target: "/mnt/data"},
			},
			wantStr: "ext4:/dev/vdb:/mnt/data",
		},
		{
			name:   "erofs mount with options",
			inputs: []string{"erofs:/dev/vda:/mnt/rootfs:ro,noatime"},
			want: vmMounts{
				{mountType: "erofs", source: "/dev/vda", target: "/mnt/rootfs", options: []string{"ro", "noatime"}},
			},
			wantStr: "erofs:/dev/vda:/mnt/rootfs:ro,noatime",
		},
		{
			name:   "multiple mounts",
			inputs: []string{"virtiofs:foo:/mnt/foo", "erofs:/dev/vda:/mnt/rootfs"},
			want: vmMounts{
				{mountType: "virtiofs", source: "foo", target: "/mnt/foo"},
				{mountType: "erofs", source: "/dev/vda", target: "/mnt/rootfs"},
			},
			wantStr: "virtiofs:foo:/mnt/foo,erofs:/dev/vda:/mnt/rootfs",
		},
		{
			name:   "mixed mount types",
			inputs: []string{"virtiofs:bind-tag:/mnt/bind", "erofs:/dev/vda:/mnt/erofs", "ext4:/dev/vdb:/mnt/ext4"},
			want: vmMounts{
				{mountType: "virtiofs", source: "bind-tag", target: "/mnt/bind"},
				{mountType: "erofs", source: "/dev/vda", target: "/mnt/erofs"},
				{mountType: "ext4", source: "/dev/vdb", target: "/mnt/ext4"},
			},
			wantStr: "virtiofs:bind-tag:/mnt/bind,erofs:/dev/vda:/mnt/erofs,ext4:/dev/vdb:/mnt/ext4",
		},
		{
			name:   "mount with nested path",
			inputs: []string{"virtiofs:config:/mnt/etc/config"},
			want: vmMounts{
				{mountType: "virtiofs", source: "config", target: "/mnt/etc/config"},
			},
			wantStr: "virtiofs:config:/mnt/etc/config",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var m vmMounts
			for _, input := range tc.inputs {
				err := m.Set(input)
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, m)
			// Try to convert back the parsed struct into a string to check if it matches the expected output.
			assert.Equal(t, tc.wantStr, m.String())
		})
	}
}

func TestParseMountsError(t *testing.T) {
	testcases := []struct {
		name  string
		input string
	}{
		{
			name:  "missing target",
			input: "virtiofs:foo",
		},
		{
			name:  "empty type",
			input: ":foo:/mnt/foo",
		},
		{
			name:  "empty source",
			input: "virtiofs::/mnt/foo",
		},
		{
			name:  "empty target",
			input: "virtiofs:foo:",
		},
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "invalid type",
			input: "nfs:server:/mnt/nfs",
		},
		{
			name:  "old format (tag:target)",
			input: "foo:/mnt/foo",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var m vmMounts
			err := m.Set(tc.input)
			assert.Error(t, err)
		})
	}
}

// TestMountString tests the String method of vmMount
func TestMountString(t *testing.T) {
	testcases := []struct {
		name string
		m    vmMount
		want string
	}{
		{
			name: "without options",
			m:    vmMount{mountType: "virtiofs", source: "tag", target: "/mnt/path"},
			want: "virtiofs:tag:/mnt/path",
		},
		{
			name: "with single option",
			m:    vmMount{mountType: "erofs", source: "/dev/vda", target: "/mnt/rootfs", options: []string{"ro"}},
			want: "erofs:/dev/vda:/mnt/rootfs:ro",
		},
		{
			name: "with multiple options",
			m:    vmMount{mountType: "ext4", source: "/dev/vdb", target: "/mnt/data", options: []string{"rw", "noatime", "nodiratime"}},
			want: "ext4:/dev/vdb:/mnt/data:rw,noatime,nodiratime",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.m.String())
		})
	}
}
