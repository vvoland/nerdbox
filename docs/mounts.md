# Mounts

Nerdbox supports various mount types for containers running inside the VM:

- **Bind mounts**: Share host directories/files with containers via virtio-fs
- **EROFS mounts**: Mount read-only EROFS filesystem images via virtio block devices
- **ext4 mounts**: Mount ext4 filesystem images via virtio block devices

## Bind Mounts

Bind mounts share paths from the host into containers running inside the VM.
They are implemented using virtio-fs.

### How It Works

When a bind mount is specified in the container spec:

1. The shim transforms the bind mount into a virtio-fs share
2. The host path is shared with the VM via virtio-fs with a unique tag
3. Inside the VM, virtio-fs is mounted at a temporary location (`/mnt/bind-{hash}`)
4. The container runtime bind-mounts from that location into the container

### Directory Bind Mounts

For directory bind mounts, the directory is shared directly via virtio-fs:

```
Host: /host/data/ → virtiofs share → VM: /mnt/bind-{hash}/ → Container: /container/data/
```

### File Bind Mounts

When bind-mounting a single file, nerdbox shares the **parent directory** of
the file via virtio-fs, then bind-mounts the specific file into the container:

```
Host: /host/config/app.yaml
  ↓
virtiofs shares: /host/config/  (parent directory)
  ↓
VM: /mnt/bind-{hash}/app.yaml
  ↓
Container: /container/app.yaml
```

### Security Implications for File Bind Mounts

When using file bind mounts, be aware that the **entire parent directory** is
exposed to the VM, not just the single file. This has security implications if
the VM is considered a security boundary:

- All files in the parent directory become accessible to the VM
- If an attacker compromises the VM, they can access any file in that directory
- Sensitive files that happen to be siblings of the bind-mounted file are exposed

**Recommendations:**

- Avoid bind-mounting files from directories containing secrets, credentials,
  or sensitive data
- If the VM is treated as a security boundary, audit what gets exposed when
  using file bind mounts
- Place files intended for bind-mounting in dedicated directories with no other
  sensitive content
- Consider using directory bind mounts with only the necessary files instead of
  file bind mounts

## EROFS Mounts

EROFS (Enhanced Read-Only File System) mounts allow you to mount read-only
filesystem images directly as container volumes.

### How It Works

When an EROFS mount is specified in the container spec:

1. The shim passes the EROFS image file to the VM as a virtio block device
2. Inside the VM, vminitd mounts the block device at a temporary location (`/mnt/erofs-{hash}`)
3. The container runtime bind-mounts from that location into the container

### Example OCI Spec Mount

```json
{
  "type": "erofs",
  "source": "/path/to/image.erofs",
  "destination": "/data",
  "options": ["ro", "noatime"]
}
```

### Use Cases

- Sharing large read-only datasets across containers
- Distributing application dependencies as compressed filesystem images
- Implementing content-addressed storage for container volumes

## ext4 Mounts

ext4 mounts allow you to mount ext4 filesystem images as container volumes,
supporting both read-only and read-write access.

### How It Works

When an ext4 mount is specified in the container spec:

1. The shim passes the ext4 image file to the VM as a virtio block device
2. Inside the VM, vminitd mounts the block device at a temporary location (`/mnt/ext4-{hash}`)
3. The container runtime bind-mounts from that location into the container

### Example OCI Spec Mount

```json
{
  "type": "ext4",
  "source": "/path/to/image.ext4",
  "destination": "/data",
  "options": ["rw"]
}
```

For read-only access:

```json
{
  "type": "ext4",
  "source": "/path/to/image.ext4",
  "destination": "/data",
  "options": ["ro"]
}
```

### Use Cases

- Persistent storage for container data
- Pre-populated data volumes
- Database storage with ext4 features (journaling, etc.)

## Mount Type Prefixes

Nerdbox supports special mount type prefixes that enable advanced mount
configurations:

### `format/` Prefix

The `format/` prefix indicates that the mount options may contain template
expressions that reference other mounts. This is primarily used for overlay
filesystems where the lower directories need to reference previously mounted
erofs layers.

Example: `format/erofs`, `format/ext4`, `format/overlay`

### `mkdir/` Prefix

The `mkdir/` prefix indicates that directories should be created before
mounting. This can be combined with `format/`.

Example: `mkdir/overlay`, `format/mkdir/overlay`

## Disk Device Allocation

Virtio block devices are assigned letters sequentially (`/dev/vda`, `/dev/vdb`,
etc.). The allocation order is:

1. **Rootfs disks** - erofs/ext4 layers for the container root filesystem
2. **Spec mount disks** - erofs/ext4 mounts specified in the container spec

This ensures that device paths are predictable and don't conflict between
rootfs and volume mounts.

## vminitd Mount Format

When passing mounts to vminitd, the shim uses the following format:

```
-mount=type:source:target[:options]
```

Where:
- `type` is the filesystem type (`virtiofs`, `erofs`, `ext4`)
- `source` is:
  - For `virtiofs`: the virtio-fs tag
  - For `erofs`/`ext4`: the block device path (e.g., `/dev/vda`)
- `target` is the mount point path in the VM
- `options` (optional) are comma-separated mount options

Examples:
```
-mount=virtiofs:bind-abc123:/mnt/bind-abc123
-mount=erofs:/dev/vda:/mnt/erofs-abc123:ro,noatime
-mount=ext4:/dev/vdb:/mnt/ext4-abc123:rw
```

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                           Host                                      │
├─────────────────────────────────────────────────────────────────────┤
│  Container Spec                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ Mounts:                                                      │   │
│  │   - type: bind, source: /host/data, dest: /container/data   │   │
│  │   - type: erofs, source: /images/app.erofs, dest: /app      │   │
│  │   - type: ext4, source: /volumes/db.ext4, dest: /var/lib/db │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                              │                                      │
│                              ▼                                      │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                    Shim (bindMounter)                        │   │
│  │  - Transform bind → virtiofs share                          │   │
│  │  - Transform erofs → virtio block device                    │   │
│  │  - Transform ext4 → virtio block device                     │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                              │                                      │
└──────────────────────────────┼──────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────┐
│                            VM                                       │
├─────────────────────────────────────────────────────────────────────┤
│  vminitd receives:                                                  │
│    -mount=virtiofs:bind-xxx:/mnt/bind-xxx                          │
│    -mount=erofs:/dev/vda:/mnt/erofs-xxx                            │
│    -mount=ext4:/dev/vdb:/mnt/ext4-xxx                              │
│                              │                                      │
│                              ▼                                      │
│  Pre-mounts filesystems:                                           │
│    /mnt/bind-xxx  ← virtiofs mount                                 │
│    /mnt/erofs-xxx ← erofs mount from /dev/vda                      │
│    /mnt/ext4-xxx  ← ext4 mount from /dev/vdb                       │
│                              │                                      │
│                              ▼                                      │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                    Container (crun/runc)                     │   │
│  │  OCI Spec Mounts (transformed):                              │   │
│  │    - bind: /mnt/bind-xxx → /container/data                   │   │
│  │    - bind: /mnt/erofs-xxx → /app                             │   │
│  │    - bind: /mnt/ext4-xxx → /var/lib/db                       │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```
