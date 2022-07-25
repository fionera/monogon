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

package crypt

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"source.monogon.dev/metropolis/pkg/devicemapper"
)

func readDataSectors(path string) (uint64, error) {
	integrityPartition, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer integrityPartition.Close()
	// Based on structure defined in
	//   https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/drivers/md/dm-integrity.c#n59
	if _, err := integrityPartition.Seek(16, 0); err != nil {
		return 0, err
	}
	var providedDataSectors uint64
	if err := binary.Read(integrityPartition, binary.LittleEndian, &providedDataSectors); err != nil {
		return 0, err
	}
	return providedDataSectors, nil
}

// cryptMap maps an encrypted device (node) at baseName to a
// decrypted device at /dev/$name using the given encryptionKey
func CryptMap(name string, baseName string, encryptionKey []byte) error {
	return cryptMap(name, baseName, encryptionKey, true)
}

func cryptMap(name string, baseName string, encryptionKey []byte, enableJournal bool) error {
	integritySectors, err := readDataSectors(baseName)
	if err != nil {
		return fmt.Errorf("failed to read the number of usable sectors on the integrity device: %w", err)
	}

	integrityDevName := fmt.Sprintf("/dev/%v-integrity", name)
	integrityDMName := fmt.Sprintf("%v-integrity", name)
	mode := "D"
	if enableJournal {
		mode = "J"
	}
	integrityDev, err := devicemapper.CreateActiveDevice(integrityDMName, false, []devicemapper.Target{
		devicemapper.Target{
			Length:     integritySectors,
			Type:       "integrity",
			Parameters: []string{baseName, "0", "28", mode, "1", "journal_sectors:1024"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create Integrity device: %w", err)
	}
	if err := unix.Mknod(integrityDevName, 0600|unix.S_IFBLK, int(integrityDev)); err != nil {
		unix.Unlink(integrityDevName)
		devicemapper.RemoveDevice(integrityDMName)
		return fmt.Errorf("failed to create integrity device node: %w", err)
	}

	cryptDevName := fmt.Sprintf("/dev/%v", name)
	cryptDev, err := devicemapper.CreateActiveDevice(name, false, []devicemapper.Target{
		devicemapper.Target{
			Length:     integritySectors,
			Type:       "crypt",
			Parameters: []string{"capi:gcm(aes)-random", hex.EncodeToString(encryptionKey), "0", integrityDevName, "0", "1", "integrity:28:aead", "no_read_workqueue", "no_write_workqueue"},
		},
	})
	if err != nil {
		unix.Unlink(integrityDevName)
		devicemapper.RemoveDevice(integrityDMName)
		return fmt.Errorf("failed to create crypt device: %w", err)
	}
	if err := unix.Mknod(cryptDevName, 0600|unix.S_IFBLK, int(cryptDev)); err != nil {
		unix.Unlink(cryptDevName)
		devicemapper.RemoveDevice(name)

		unix.Unlink(integrityDevName)
		devicemapper.RemoveDevice(integrityDMName)
		return fmt.Errorf("failed to create crypt device node: %w", err)
	}
	return nil
}

func cryptUnmap(name string, baseName string) error {
	integrityDevName := fmt.Sprintf("/dev/%v-integrity", name)
	if err := unix.Unlink(integrityDevName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete integrity device inode: %w", err)
	}
	cryptDevName := fmt.Sprintf("/dev/%v", name)
	if err := unix.Unlink(cryptDevName); err != nil {
		return fmt.Errorf("failed to delete crypt device inode: %w", err)
	}
	integrityDMName := fmt.Sprintf("%v-integrity", name)
	if err := devicemapper.RemoveDevice(name); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("failed to remove dm-crypt device: %w", err)
	}
	if err := devicemapper.RemoveDevice(integrityDMName); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("failed to remove dm-integrity device: %w", err)
	}
	return nil
}

// cryptInit initializes a new encrypted block device. This can take a long
// time since all bytes on the mapped block device need to be zeroed.
func CryptInit(name, baseName string, encryptionKey []byte) error {
	integrityPartition, err := os.OpenFile(baseName, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer integrityPartition.Close()
	zeroed512BBuf := make([]byte, 4096)
	if _, err := integrityPartition.Write(zeroed512BBuf); err != nil {
		return fmt.Errorf("failed to wipe header: %w", err)
	}
	integrityPartition.Close()

	integrityDMName := fmt.Sprintf("%v-integrity", name)
	_, err = devicemapper.CreateActiveDevice(integrityDMName, false, []devicemapper.Target{
		{
			Length:     1,
			Type:       "integrity",
			Parameters: []string{baseName, "0", "28", "J", "1", "journal_sectors:1024"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create discovery integrity device: %w", err)
	}
	if err := devicemapper.RemoveDevice(integrityDMName); err != nil {
		return fmt.Errorf("failed to remove discovery integrity device: %w", err)
	}

	// First, map the device without journal. Zeroing with journal is extremely
	// slow as it transforms sequential IO into random IO and also consumes
	// twice the write operations. This is fine as if we abort here we'll
	// reinitialize the whole device so the reliability is of no concern.
	if err := cryptMap(name, baseName, encryptionKey, false); err != nil {
		return err
	}

	blkdev, err := os.OpenFile(fmt.Sprintf("/dev/%v", name), unix.O_DIRECT|os.O_WRONLY, 0000)
	if err != nil {
		return fmt.Errorf("failed to open new encrypted device for zeroing: %w", err)
	}
	defer blkdev.Close()
	blockSize, err := unix.IoctlGetUint32(int(blkdev.Fd()), unix.BLKSSZGET)
	zeroedBuf := make([]byte, blockSize*256) // Make it faster
	for {
		_, err := blkdev.Write(zeroedBuf)
		if e, ok := err.(*os.PathError); ok && e.Err == syscall.ENOSPC {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to zero-initalize new encrypted device: %w", err)
		}
	}
	blkdev.Close()

	// Now, unmap the non-journaled device and remap it with journaling for
	// further use.
	if err := cryptUnmap(name, baseName); err != nil {
		return fmt.Errorf("failed to unmap temporary encrypted block device: %w", err)
	}
	if err := cryptMap(name, baseName, encryptionKey, true); err != nil {
		return fmt.Errorf("failed to map initialized encrypted device: %w", err)
	}

	return nil
}
