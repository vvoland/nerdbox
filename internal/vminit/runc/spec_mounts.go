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

package runc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// specMount represents an active mount from processing OCI spec mounts.
type specMount struct {
	mount      specs.Mount
	mountPoint string
	mountedAt  time.Time
}

// processSpecMounts processes OCI spec mounts that have special types like
// format/ext4, format/mkdir/bind, etc. These mounts need to be pre-processed
// before crun can handle them.
//
// It returns a cleanup function that should be called to unmount any mounts
// that were set up during processing.
func processSpecMounts(ctx context.Context, bundlePath string) (cleanup func(), retErr error) {
	specPath := filepath.Join(bundlePath, "config.json")
	specData, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return nil, fmt.Errorf("unmarshaling spec: %w", err)
	}

	// Track mounts that we set up for cleanup
	var activeMounts []specMount

	// Create a mounts directory for intermediate mounts
	mdir := filepath.Join(bundlePath, "spec-mounts")
	if err := os.MkdirAll(mdir, 0755); err != nil {
		return nil, fmt.Errorf("creating spec-mounts dir: %w", err)
	}

	cleanup = func() {
		// Unmount in reverse order
		for i := len(activeMounts) - 1; i >= 0; i-- {
			am := activeMounts[i]
			if err := mount.UnmountAll(am.mountPoint, 0); err != nil {
				log.G(ctx).WithError(err).WithField("mountpoint", am.mountPoint).Warn("failed to cleanup spec mount")
			}
		}
	}

	defer func() {
		if retErr != nil && cleanup != nil {
			cleanup()
			cleanup = nil
		}
	}()

	modified := false
	var newMounts []specs.Mount

	for i, m := range spec.Mounts {
		// Check if this mount needs special processing
		if !strings.HasPrefix(m.Type, "format/") && !strings.HasPrefix(m.Type, "mkdir/") {
			newMounts = append(newMounts, m)
			continue
		}

		log.G(ctx).WithFields(log.Fields{
			"type":        m.Type,
			"source":      m.Source,
			"destination": m.Destination,
		}).Debug("processing spec mount")

		modified = true

		// Process format/ prefix - resolve templates
		if t, ok := strings.CutPrefix(m.Type, "format/"); ok {
			m.Type = t

			// Resolve templates in options
			for j, o := range m.Options {
				if resolved, err := resolveTemplate(o, activeMounts); err != nil {
					return nil, fmt.Errorf("resolving template in option %q: %w", o, err)
				} else if resolved != o {
					m.Options[j] = resolved
				}
			}

			// Resolve template in source
			if resolved, err := resolveTemplate(m.Source, activeMounts); err != nil {
				return nil, fmt.Errorf("resolving template in source %q: %w", m.Source, err)
			} else {
				m.Source = resolved
			}
		}

		// Process mkdir/ prefix - create directories before mount
		if t, ok := strings.CutPrefix(m.Type, "mkdir/"); ok {
			m.Type = t

			var filteredOptions []string
			for _, o := range m.Options {
				if strings.HasPrefix(o, "X-containerd.mkdir.") {
					prefix := "X-containerd.mkdir.path="
					if !strings.HasPrefix(o, prefix) {
						return nil, fmt.Errorf("unknown mkdir mount option %q", o)
					}
					pathSpec := o[len(prefix):]
					parts := strings.SplitN(pathSpec, ":", 4)
					if len(parts) >= 1 {
						dir := parts[0]
						log.G(ctx).WithField("dir", dir).Debug("creating directory for spec mount")
						if err := os.MkdirAll(dir, 0755); err != nil {
							return nil, fmt.Errorf("creating directory %q: %w", dir, err)
						}
					}
				} else {
					filteredOptions = append(filteredOptions, o)
				}
			}
			m.Options = filteredOptions
		}

		// If this is a block device mount (ext4, etc.), we need to mount it
		// and track it for template resolution in subsequent mounts
		if isBlockDeviceMount(m.Type) {
			mountPoint := filepath.Join(mdir, fmt.Sprintf("%d", i))
			if err := os.MkdirAll(mountPoint, 0755); err != nil {
				return nil, fmt.Errorf("creating mount point: %w", err)
			}

			log.G(ctx).WithFields(log.Fields{
				"type":       m.Type,
				"source":     m.Source,
				"mountPoint": mountPoint,
			}).Debug("mounting block device for spec mount")

			mnt := mount.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			}
			if err := mnt.Mount(mountPoint); err != nil {
				return nil, fmt.Errorf("mounting %s at %s: %w", m.Source, mountPoint, err)
			}

			activeMounts = append(activeMounts, specMount{
				mount:      m,
				mountPoint: mountPoint,
				mountedAt:  time.Now(),
			})

			// Don't add this mount to the spec - it's now mounted at a different location
			// and will be referenced by subsequent bind mounts
			continue
		}

		newMounts = append(newMounts, m)
	}

	if !modified {
		return func() {}, nil
	}

	// Write the modified spec back
	spec.Mounts = newMounts
	newSpecData, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshaling modified spec: %w", err)
	}

	if err := os.WriteFile(specPath, newSpecData, 0644); err != nil {
		return nil, fmt.Errorf("writing modified spec: %w", err)
	}

	log.G(ctx).WithField("mounts", newMounts).Debug("wrote modified spec with processed mounts")

	return cleanup, nil
}

// isBlockDeviceMount returns true if the mount type is for a block device filesystem.
func isBlockDeviceMount(mountType string) bool {
	switch mountType {
	case "ext4", "ext3", "ext2", "xfs", "btrfs", "erofs":
		return true
	default:
		return false
	}
}

// resolveTemplate resolves Go template strings using the active mounts.
func resolveTemplate(s string, activeMounts []specMount) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}

	fm := template.FuncMap{
		"source": func(i int) (string, error) {
			if i < 0 || i >= len(activeMounts) {
				return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(activeMounts))
			}
			return activeMounts[i].mount.Source, nil
		},
		"mount": func(i int) (string, error) {
			if i < 0 || i >= len(activeMounts) {
				return "", fmt.Errorf("index out of bounds: %d, has %d active mounts", i, len(activeMounts))
			}
			return activeMounts[i].mountPoint, nil
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
