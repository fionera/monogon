// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package localstorage

import (
	"crypto/rand"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"

	"source.monogon.dev/metropolis/node/core/localstorage/crypt"
	"source.monogon.dev/metropolis/node/core/localstorage/declarative"
	"source.monogon.dev/metropolis/pkg/tpm"
	cpb "source.monogon.dev/metropolis/proto/common"
	ppb "source.monogon.dev/metropolis/proto/private"
)

var keySize uint16 = 256 / 8

// MountExisting mounts the node data partition with the given cluster unlock key.
// It automatically unseals the node unlock key from the TPM.
func (d *DataDirectory) MountExisting(config *ppb.SealedConfiguration, clusterUnlockKey []byte) error {
	var mode crypt.Mode
	switch config.StorageSecurity {
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_INSECURE:
		mode = crypt.ModeInsecure
		if len(clusterUnlockKey) != 0 {
			return fmt.Errorf("storage security set to insecure, but cluster unlock key received")
		}
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_ENCRYPTED:
		mode = crypt.ModeEncrypted
		if len(clusterUnlockKey) != 32 {
			return fmt.Errorf("storage security set to encrypted, but invalid cluster unlock key received")
		}
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_AUTHENTICATED_ENCRYPTED:
		mode = crypt.ModeEncryptedAuthenticated
		if len(clusterUnlockKey) != 32 {
			return fmt.Errorf("storage security set to encrypted and authenticated, but invalid cluster unlock key received")
		}
	default:
		return fmt.Errorf("invalid storage security in sealed configuration: %d", config.StorageSecurity)
	}

	d.flagLock.Lock()
	defer d.flagLock.Unlock()

	if !d.canMount {
		return fmt.Errorf("cannot mount yet (root not ready?)")
	}
	if d.mounted {
		return fmt.Errorf("already mounted")
	}
	d.mounted = true

	var key []byte
	if mode != crypt.ModeInsecure {
		key = make([]byte, keySize)
		for i := uint16(0); i < keySize; i++ {
			key[i] = config.NodeUnlockKey[i] ^ clusterUnlockKey[i]
		}
	}

	target, err := crypt.Map("data", crypt.NodeDataRawPath, key, mode)
	if err != nil {
		return err
	}
	if err := d.mount(target); err != nil {
		return err
	}
	return nil
}

// MountNew initializes the node data partition and returns the cluster unlock
// key. It seals the local portion into the TPM. This is a potentially slow
// operation since it touches the whole partition.
func (d *DataDirectory) MountNew(config *ppb.SealedConfiguration, security cpb.NodeStorageSecurity) ([]byte, error) {
	d.flagLock.Lock()
	defer d.flagLock.Unlock()

	if !d.canMount {
		return nil, fmt.Errorf("cannot mount yet (root not ready?)")
	}
	if d.mounted {
		return nil, fmt.Errorf("already mounted")
	}
	d.mounted = true

	var mode crypt.Mode
	switch security {
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_AUTHENTICATED_ENCRYPTED:
		mode = crypt.ModeEncryptedAuthenticated
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_ENCRYPTED:
		mode = crypt.ModeEncrypted
	case cpb.NodeStorageSecurity_NODE_STORAGE_SECURITY_INSECURE:
		mode = crypt.ModeInsecure
	default:
		return nil, fmt.Errorf("invalid node storage security: %d", security)
	}
	config.StorageSecurity = security

	var nodeUnlockKey, clusterUnlockKey, key []byte

	// Generate keys unless we're in insecure mode.
	if mode != crypt.ModeInsecure {
		var err error
		if tpm.IsInitialized() {
			nodeUnlockKey, err = tpm.GenerateSafeKey(keySize)
		} else {
			nodeUnlockKey = make([]byte, keySize)
			_, err = rand.Read(nodeUnlockKey)
		}
		if err != nil {
			return nil, fmt.Errorf("generating node unlock key: %w", err)
		}
		if tpm.IsInitialized() {
			clusterUnlockKey, err = tpm.GenerateSafeKey(keySize)
		} else {
			clusterUnlockKey = make([]byte, keySize)
			_, err = rand.Read(clusterUnlockKey)
		}
		if err != nil {
			return nil, fmt.Errorf("generating cluster unlock key: %w", err)
		}

		// The actual key is generated by XORing together the nodeUnlockKey and the
		// globalUnlockKey This provides us with a mathematical guarantee that the
		// resulting key cannot be recovered without knowledge of both parts.
		key = make([]byte, keySize)
		for i := uint16(0); i < keySize; i++ {
			key[i] = nodeUnlockKey[i] ^ clusterUnlockKey[i]
		}
	}

	target, err := crypt.Init("data", crypt.NodeDataRawPath, key, mode)
	if err != nil {
		return nil, fmt.Errorf("initializing encrypted block device: %w", err)
	}
	mkfsCmd := exec.Command("/bin/mkfs.xfs", "-qKf", target)
	if _, err := mkfsCmd.Output(); err != nil {
		return nil, fmt.Errorf("formatting encrypted block device: %w", err)
	}

	if err := d.mount(target); err != nil {
		return nil, fmt.Errorf("mounting: %w", err)
	}

	// TODO(q3k): do this automatically?
	for _, d := range []declarative.DirectoryPlacement{
		d.Etcd, d.Etcd.Data, d.Etcd.PeerPKI,
		d.Containerd,
		d.Kubernetes,
		d.Kubernetes.Kubelet, d.Kubernetes.Kubelet.Plugins, d.Kubernetes.Kubelet.PluginsRegistry,
		d.Kubernetes.ClusterNetworking,
		d.Node,
		d.Node.Credentials,
		d.Volumes,
	} {
		err := d.MkdirAll(0700)
		if err != nil {
			return nil, fmt.Errorf("creating directory failed: %w", err)
		}
	}

	config.NodeUnlockKey = nodeUnlockKey

	return clusterUnlockKey, nil
}

func (d *DataDirectory) mount(path string) error {
	// TODO(T965): MS_NODEV should definitely be set on the data partition, but as long as the kubelet root
	// is on there, we can't do it.
	if err := unix.Mount(path, d.FullPath(), "xfs", unix.MS_NOEXEC, "pquota"); err != nil {
		return fmt.Errorf("mounting data directory: %w", err)
	}
	return nil
}
