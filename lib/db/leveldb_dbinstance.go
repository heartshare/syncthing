// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package db

import (
	"bytes"
	"encoding/binary"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/sync"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
)

type deletionHandler func(t readWriteTransaction, folder, device, name []byte, dbi iterator.Iterator)

type Instance struct {
	committed int64 // this must be the first attribute in the struct to ensure 64 bit alignment on 32 bit plaforms
	*leveldb.DB
	location  string
	folderIdx *smallIndex
	deviceIdx *smallIndex
}

const (
	keyPrefixLen = 1
	keyFolderLen = 4 // indexed
	keyDeviceLen = 4 // indexed
	keyHashLen   = 32
)

func Open(file string) (*Instance, error) {
	opts := &opt.Options{
		OpenFilesCacheCapacity: 100,
		WriteBuffer:            4 << 20,
	}

	db, err := leveldb.OpenFile(file, opts)
	if leveldbIsCorrupted(err) {
		db, err = leveldb.RecoverFile(file, opts)
	}
	if leveldbIsCorrupted(err) {
		// The database is corrupted, and we've tried to recover it but it
		// didn't work. At this point there isn't much to do beyond dropping
		// the database and reindexing...
		l.Infoln("Database corruption detected, unable to recover. Reinitializing...")
		if err := os.RemoveAll(file); err != nil {
			return nil, err
		}
		db, err = leveldb.OpenFile(file, opts)
	}
	if err != nil {
		return nil, err
	}

	return newDBInstance(db, file), nil
}

func OpenMemory() *Instance {
	db, _ := leveldb.Open(storage.NewMemStorage(), nil)
	return newDBInstance(db, "<memory>")
}

func newDBInstance(db *leveldb.DB, location string) *Instance {
	i := &Instance{
		DB:       db,
		location: location,
	}
	i.folderIdx = newSmallIndex(i, []byte{KeyTypeFolderIdx})
	i.deviceIdx = newSmallIndex(i, []byte{KeyTypeDeviceIdx})
	return i
}

// Committed returns the number of items committed to the database since startup
func (db *Instance) Committed() int64 {
	return atomic.LoadInt64(&db.committed)
}

// Location returns the filesystem path where the database is stored
func (db *Instance) Location() string {
	return db.location
}

func (db *Instance) genericReplace(folder, device []byte, fs []protocol.FileInfo, localSize, globalSize *sizeTracker, deleteFn deletionHandler) {
	sort.Sort(fileList(fs)) // sort list on name, same as in the database

	t := db.newReadWriteTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.deviceKey(folder, device, nil)[:keyPrefixLen+keyFolderLen+keyDeviceLen]), nil)
	defer dbi.Release()

	moreDb := dbi.Next()
	fsi := 0

	isLocalDevice := bytes.Equal(device, protocol.LocalDeviceID[:])
	for {
		var newName, oldName []byte
		moreFs := fsi < len(fs)

		if !moreDb && !moreFs {
			break
		}

		if moreFs {
			newName = []byte(fs[fsi].Name)
		}

		if moreDb {
			oldName = db.deviceKeyName(dbi.Key())
		}

		cmp := bytes.Compare(newName, oldName)

		l.Debugf("generic replace; folder=%q device=%v moreFs=%v moreDb=%v cmp=%d newName=%q oldName=%q", folder, protocol.DeviceIDFromBytes(device), moreFs, moreDb, cmp, newName, oldName)

		switch {
		case moreFs && (!moreDb || cmp == -1):
			l.Debugln("generic replace; missing - insert")
			// Database is missing this file. Insert it.
			t.insertFile(folder, device, fs[fsi])
			if isLocalDevice {
				localSize.addFile(fs[fsi])
			}
			if fs[fsi].IsInvalid() {
				t.removeFromGlobal(folder, device, newName, globalSize)
			} else {
				t.updateGlobal(folder, device, fs[fsi], globalSize)
			}
			fsi++

		case moreFs && moreDb && cmp == 0:
			// File exists on both sides - compare versions. We might get an
			// update with the same version if a device has marked a file as
			// invalid, so handle that too.
			l.Debugln("generic replace; exists - compare")
			var ef FileInfoTruncated
			ef.Unmarshal(dbi.Value())
			if !fs[fsi].Version.Equal(ef.Version) || fs[fsi].Invalid != ef.Invalid {
				l.Debugln("generic replace; differs - insert")
				t.insertFile(folder, device, fs[fsi])
				if isLocalDevice {
					localSize.removeFile(ef)
					localSize.addFile(fs[fsi])
				}
				if fs[fsi].IsInvalid() {
					t.removeFromGlobal(folder, device, newName, globalSize)
				} else {
					t.updateGlobal(folder, device, fs[fsi], globalSize)
				}
			} else {
				l.Debugln("generic replace; equal - ignore")
			}

			fsi++
			moreDb = dbi.Next()

		case moreDb && (!moreFs || cmp == 1):
			l.Debugln("generic replace; exists - remove")
			deleteFn(t, folder, device, oldName, dbi)
			moreDb = dbi.Next()
		}

		// Write out and reuse the batch every few records, to avoid the batch
		// growing too large and thus allocating unnecessarily much memory.
		t.checkFlush()
	}
}

func (db *Instance) replace(folder, device []byte, fs []protocol.FileInfo, localSize, globalSize *sizeTracker) {
	db.genericReplace(folder, device, fs, localSize, globalSize, func(t readWriteTransaction, folder, device, name []byte, dbi iterator.Iterator) {
		// Database has a file that we are missing. Remove it.
		l.Debugf("delete; folder=%q device=%v name=%q", folder, protocol.DeviceIDFromBytes(device), name)
		t.removeFromGlobal(folder, device, name, globalSize)
		t.Delete(dbi.Key())
	})
}

func (db *Instance) updateFiles(folder, device []byte, fs []protocol.FileInfo, localSize, globalSize *sizeTracker) {
	t := db.newReadWriteTransaction()
	defer t.close()

	var fk []byte
	isLocalDevice := bytes.Equal(device, protocol.LocalDeviceID[:])
	for _, f := range fs {
		name := []byte(f.Name)
		fk = db.deviceKeyInto(fk[:cap(fk)], folder, device, name)
		bs, err := t.Get(fk, nil)
		if err == leveldb.ErrNotFound {
			if isLocalDevice {
				localSize.addFile(f)
			}

			t.insertFile(folder, device, f)
			if f.IsInvalid() {
				t.removeFromGlobal(folder, device, name, globalSize)
			} else {
				t.updateGlobal(folder, device, f, globalSize)
			}
			continue
		}

		var ef FileInfoTruncated
		err = ef.Unmarshal(bs)
		if err != nil {
			panic(err)
		}
		// The Invalid flag might change without the version being bumped.
		if !ef.Version.Equal(f.Version) || ef.Invalid != f.Invalid {
			if isLocalDevice {
				localSize.removeFile(ef)
				localSize.addFile(f)
			}

			t.insertFile(folder, device, f)
			if f.IsInvalid() {
				t.removeFromGlobal(folder, device, name, globalSize)
			} else {
				t.updateGlobal(folder, device, f, globalSize)
			}
		}

		// Write out and reuse the batch every few records, to avoid the batch
		// growing too large and thus allocating unnecessarily much memory.
		t.checkFlush()
	}
}

func (db *Instance) withHave(folder, device, prefix []byte, truncate bool, fn Iterator) {
	t := db.newReadOnlyTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.deviceKey(folder, device, prefix)[:keyPrefixLen+keyFolderLen+keyDeviceLen+len(prefix)]), nil)
	defer dbi.Release()

	slashedPrefix := prefix
	if !bytes.HasSuffix(prefix, []byte{'/'}) {
		slashedPrefix = append(slashedPrefix, '/')
	}

	for dbi.Next() {
		name := db.deviceKeyName(dbi.Key())
		if len(prefix) > 0 && !bytes.Equal(name, prefix) && !bytes.HasPrefix(name, slashedPrefix) {
			return
		}

		// The iterator function may keep a reference to the unmarshalled
		// struct, which in turn references the buffer it was unmarshalled
		// from. dbi.Value() just returns an internal slice that it reuses, so
		// we need to copy it.
		f, err := unmarshalTrunc(append([]byte{}, dbi.Value()...), truncate)
		if err != nil {
			panic(err)
		}
		if cont := fn(f); !cont {
			return
		}
	}
}

func (db *Instance) withAllFolderTruncated(folder []byte, fn func(device []byte, f FileInfoTruncated) bool) {
	t := db.newReadWriteTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.deviceKey(folder, nil, nil)[:keyPrefixLen+keyFolderLen]), nil)
	defer dbi.Release()

	for dbi.Next() {
		device := db.deviceKeyDevice(dbi.Key())
		var f FileInfoTruncated
		// The iterator function may keep a reference to the unmarshalled
		// struct, which in turn references the buffer it was unmarshalled
		// from. dbi.Value() just returns an internal slice that it reuses, so
		// we need to copy it.
		err := f.Unmarshal(append([]byte{}, dbi.Value()...))
		if err != nil {
			panic(err)
		}

		switch f.Name {
		case "", ".", "..", "/": // A few obviously invalid filenames
			l.Infof("Dropping invalid filename %q from database", f.Name)
			t.removeFromGlobal(folder, device, nil, nil)
			t.Delete(dbi.Key())
			t.checkFlush()
			continue
		}

		if cont := fn(device, f); !cont {
			return
		}
	}
}

func (db *Instance) getFile(folder, device, file []byte) (protocol.FileInfo, bool) {
	return getFile(db, db.deviceKey(folder, device, file))
}

func (db *Instance) getGlobal(folder, file []byte, truncate bool) (FileIntf, bool) {
	k := db.globalKey(folder, file)

	t := db.newReadOnlyTransaction()
	defer t.close()

	bs, err := t.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return nil, false
	}
	if err != nil {
		panic(err)
	}

	var vl VersionList
	err = vl.Unmarshal(bs)
	if err != nil {
		panic(err)
	}
	if len(vl.Versions) == 0 {
		l.Debugln(k)
		panic("no versions?")
	}

	k = db.deviceKey(folder, vl.Versions[0].Device, file)
	bs, err = t.Get(k, nil)
	if err != nil {
		panic(err)
	}

	fi, err := unmarshalTrunc(bs, truncate)
	if err != nil {
		panic(err)
	}
	return fi, true
}

func (db *Instance) withGlobal(folder, prefix []byte, truncate bool, fn Iterator) {
	t := db.newReadOnlyTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.globalKey(folder, prefix)), nil)
	defer dbi.Release()

	slashedPrefix := prefix
	if !bytes.HasSuffix(prefix, []byte{'/'}) {
		slashedPrefix = append(slashedPrefix, '/')
	}

	var fk []byte
	for dbi.Next() {
		var vl VersionList
		err := vl.Unmarshal(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.Versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}

		name := db.globalKeyName(dbi.Key())
		if len(prefix) > 0 && !bytes.Equal(name, prefix) && !bytes.HasPrefix(name, slashedPrefix) {
			return
		}

		fk = db.deviceKeyInto(fk[:cap(fk)], folder, vl.Versions[0].Device, name)
		bs, err := t.Get(fk, nil)
		if err != nil {
			l.Debugf("folder: %q (%x)", folder, folder)
			l.Debugf("key: %q (%x)", dbi.Key(), dbi.Key())
			l.Debugf("vl: %v", vl)
			l.Debugf("vl.Versions[0].Device: %x", vl.Versions[0].Device)
			l.Debugf("name: %q (%x)", name, name)
			l.Debugf("fk: %q", fk)
			l.Debugf("fk: %x %x %x",
				fk[keyPrefixLen:keyPrefixLen+keyFolderLen],
				fk[keyPrefixLen+keyFolderLen:keyPrefixLen+keyFolderLen+keyDeviceLen],
				fk[keyPrefixLen+keyFolderLen+keyDeviceLen:])
			panic(err)
		}

		f, err := unmarshalTrunc(bs, truncate)
		if err != nil {
			panic(err)
		}

		if cont := fn(f); !cont {
			return
		}
	}
}

func (db *Instance) availability(folder, file []byte) []protocol.DeviceID {
	k := db.globalKey(folder, file)
	bs, err := db.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return nil
	}
	if err != nil {
		panic(err)
	}

	var vl VersionList
	err = vl.Unmarshal(bs)
	if err != nil {
		panic(err)
	}

	var devices []protocol.DeviceID
	for _, v := range vl.Versions {
		if !v.Version.Equal(vl.Versions[0].Version) {
			break
		}
		n := protocol.DeviceIDFromBytes(v.Device)
		devices = append(devices, n)
	}

	return devices
}

func (db *Instance) withNeed(folder, device []byte, truncate bool, fn Iterator) {
	t := db.newReadOnlyTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.globalKey(folder, nil)[:keyPrefixLen+keyFolderLen]), nil)
	defer dbi.Release()

	var fk []byte
nextFile:
	for dbi.Next() {
		var vl VersionList
		err := vl.Unmarshal(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.Versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}

		have := false // If we have the file, any version
		need := false // If we have a lower version of the file
		var haveVersion protocol.Vector
		for _, v := range vl.Versions {
			if bytes.Equal(v.Device, device) {
				have = true
				haveVersion = v.Version
				// XXX: This marks Concurrent (i.e. conflicting) changes as
				// needs. Maybe we should do that, but it needs special
				// handling in the puller.
				need = !v.Version.GreaterEqual(vl.Versions[0].Version)
				break
			}
		}

		if need || !have {
			name := db.globalKeyName(dbi.Key())
			needVersion := vl.Versions[0].Version

		nextVersion:
			for i := range vl.Versions {
				if !vl.Versions[i].Version.Equal(needVersion) {
					// We haven't found a valid copy of the file with the needed version.
					continue nextFile
				}
				fk = db.deviceKeyInto(fk[:cap(fk)], folder, vl.Versions[i].Device, name)
				bs, err := t.Get(fk, nil)
				if err != nil {
					var id protocol.DeviceID
					copy(id[:], device)
					l.Debugf("device: %v", id)
					l.Debugf("need: %v, have: %v", need, have)
					l.Debugf("key: %q (%x)", dbi.Key(), dbi.Key())
					l.Debugf("vl: %v", vl)
					l.Debugf("i: %v", i)
					l.Debugf("fk: %q (%x)", fk, fk)
					l.Debugf("name: %q (%x)", name, name)
					panic(err)
				}

				gf, err := unmarshalTrunc(bs, truncate)
				if err != nil {
					panic(err)
				}

				if gf.IsInvalid() {
					// The file is marked invalid for whatever reason, don't use it.
					continue nextVersion
				}

				if gf.IsDeleted() && !have {
					// We don't need deleted files that we don't have
					continue nextFile
				}

				l.Debugf("need folder=%q device=%v name=%q need=%v have=%v haveV=%d globalV=%d", folder, protocol.DeviceIDFromBytes(device), name, need, have, haveVersion, vl.Versions[0].Version)

				if cont := fn(gf); !cont {
					return
				}

				// This file is handled, no need to look further in the version list
				continue nextFile
			}
		}
	}
}

func (db *Instance) ListFolders() []string {
	t := db.newReadOnlyTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix([]byte{KeyTypeGlobal}), nil)
	defer dbi.Release()

	folderExists := make(map[string]bool)
	for dbi.Next() {
		folder, ok := db.globalKeyFolder(dbi.Key())
		if ok && !folderExists[string(folder)] {
			folderExists[string(folder)] = true
		}
	}

	folders := make([]string, 0, len(folderExists))
	for k := range folderExists {
		folders = append(folders, k)
	}

	sort.Strings(folders)
	return folders
}

func (db *Instance) dropFolder(folder []byte) {
	t := db.newReadOnlyTransaction()
	defer t.close()

	// Remove all items related to the given folder from the device->file bucket
	dbi := t.NewIterator(util.BytesPrefix([]byte{KeyTypeDevice}), nil)
	for dbi.Next() {
		itemFolder := db.deviceKeyFolder(dbi.Key())
		if bytes.Equal(folder, itemFolder) {
			db.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()

	// Remove all items related to the given folder from the global bucket
	dbi = t.NewIterator(util.BytesPrefix([]byte{KeyTypeGlobal}), nil)
	for dbi.Next() {
		itemFolder, ok := db.globalKeyFolder(dbi.Key())
		if ok && bytes.Equal(folder, itemFolder) {
			db.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()
}

func (db *Instance) checkGlobals(folder []byte, globalSize *sizeTracker) {
	t := db.newReadWriteTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(db.globalKey(folder, nil)[:keyPrefixLen+keyFolderLen]), nil)
	defer dbi.Release()

	var fk []byte
	for dbi.Next() {
		gk := dbi.Key()
		var vl VersionList
		err := vl.Unmarshal(dbi.Value())
		if err != nil {
			panic(err)
		}

		// Check the global version list for consistency. An issue in previous
		// versions of goleveldb could result in reordered writes so that
		// there are global entries pointing to no longer existing files. Here
		// we find those and clear them out.

		name := db.globalKeyName(gk)
		var newVL VersionList
		for i, version := range vl.Versions {
			fk = db.deviceKeyInto(fk[:cap(fk)], folder, version.Device, name)

			_, err := t.Get(fk, nil)
			if err == leveldb.ErrNotFound {
				continue
			}
			if err != nil {
				panic(err)
			}
			newVL.Versions = append(newVL.Versions, version)

			if i == 0 {
				fi, ok := t.getFile(folder, version.Device, name)
				if !ok {
					panic("nonexistent global master file")
				}
				globalSize.addFile(fi)
			}
		}

		if len(newVL.Versions) != len(vl.Versions) {
			t.Put(dbi.Key(), mustMarshal(&newVL))
			t.checkFlush()
		}
	}
	l.Debugf("db check completed for %q", folder)
}

// ConvertSymlinkTypes should be run once only on an old database. It
// changes SYMLINK_FILE and SYMLINK_DIRECTORY types to the current SYMLINK
// type (previously SYMLINK_UNKNOWN). It does this for all devices, both
// local and remote, and does not reset delta indexes. It shouldn't really
// matter what the symlink type is, but this cleans it up for a possible
// future when SYMLINK_FILE and SYMLINK_DIRECTORY are no longer understood.
func (db *Instance) ConvertSymlinkTypes() {
	t := db.newReadWriteTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix([]byte{KeyTypeDevice}), nil)
	defer dbi.Release()

	conv := 0
	for dbi.Next() {
		var f protocol.FileInfo
		if err := f.Unmarshal(dbi.Value()); err != nil {
			// probably can't happen
			continue
		}
		if f.Type == protocol.FileInfoTypeDeprecatedSymlinkDirectory || f.Type == protocol.FileInfoTypeDeprecatedSymlinkFile {
			f.Type = protocol.FileInfoTypeSymlink
			bs, err := f.Marshal()
			if err != nil {
				panic("can't happen: " + err.Error())
			}
			t.Put(dbi.Key(), bs)
			t.checkFlush()
			conv++
		}
	}

	l.Infof("Updated symlink type for %d index entries", conv)
}

// deviceKey returns a byte slice encoding the following information:
//	   keyTypeDevice (1 byte)
//	   folder (4 bytes)
//	   device (4 bytes)
//	   name (variable size)
func (db *Instance) deviceKey(folder, device, file []byte) []byte {
	return db.deviceKeyInto(nil, folder, device, file)
}

func (db *Instance) deviceKeyInto(k []byte, folder, device, file []byte) []byte {
	reqLen := keyPrefixLen + keyFolderLen + keyDeviceLen + len(file)
	if len(k) < reqLen {
		k = make([]byte, reqLen)
	}
	k[0] = KeyTypeDevice
	binary.BigEndian.PutUint32(k[keyPrefixLen:], db.folderIdx.ID(folder))
	binary.BigEndian.PutUint32(k[keyPrefixLen+keyFolderLen:], db.deviceIdx.ID(device))
	copy(k[keyPrefixLen+keyFolderLen+keyDeviceLen:], file)
	return k[:reqLen]
}

// deviceKeyName returns the device ID from the key
func (db *Instance) deviceKeyName(key []byte) []byte {
	return key[keyPrefixLen+keyFolderLen+keyDeviceLen:]
}

// deviceKeyFolder returns the folder name from the key
func (db *Instance) deviceKeyFolder(key []byte) []byte {
	folder, ok := db.folderIdx.Val(binary.BigEndian.Uint32(key[keyPrefixLen:]))
	if !ok {
		panic("bug: lookup of nonexistent folder ID")
	}
	return folder
}

// deviceKeyDevice returns the device ID from the key
func (db *Instance) deviceKeyDevice(key []byte) []byte {
	device, ok := db.deviceIdx.Val(binary.BigEndian.Uint32(key[keyPrefixLen+keyFolderLen:]))
	if !ok {
		panic("bug: lookup of nonexistent device ID")
	}
	return device
}

// globalKey returns a byte slice encoding the following information:
//	   keyTypeGlobal (1 byte)
//	   folder (4 bytes)
//	   name (variable size)
func (db *Instance) globalKey(folder, file []byte) []byte {
	k := make([]byte, keyPrefixLen+keyFolderLen+len(file))
	k[0] = KeyTypeGlobal
	binary.BigEndian.PutUint32(k[keyPrefixLen:], db.folderIdx.ID(folder))
	copy(k[keyPrefixLen+keyFolderLen:], file)
	return k
}

// globalKeyName returns the filename from the key
func (db *Instance) globalKeyName(key []byte) []byte {
	return key[keyPrefixLen+keyFolderLen:]
}

// globalKeyFolder returns the folder name from the key
func (db *Instance) globalKeyFolder(key []byte) ([]byte, bool) {
	return db.folderIdx.Val(binary.BigEndian.Uint32(key[keyPrefixLen:]))
}

func (db *Instance) getIndexID(device, folder []byte) protocol.IndexID {
	key := db.indexIDKey(device, folder)
	cur, err := db.Get(key, nil)
	if err != nil {
		return 0
	}

	var id protocol.IndexID
	if err := id.Unmarshal(cur); err != nil {
		return 0
	}

	return id
}

func (db *Instance) setIndexID(device, folder []byte, id protocol.IndexID) {
	key := db.indexIDKey(device, folder)
	bs, _ := id.Marshal() // marshalling can't fail
	if err := db.Put(key, bs, nil); err != nil {
		panic("storing index ID: " + err.Error())
	}
}

func (db *Instance) indexIDKey(device, folder []byte) []byte {
	k := make([]byte, keyPrefixLen+keyDeviceLen+keyFolderLen)
	k[0] = KeyTypeIndexID
	binary.BigEndian.PutUint32(k[keyPrefixLen:], db.deviceIdx.ID(device))
	binary.BigEndian.PutUint32(k[keyPrefixLen+keyDeviceLen:], db.folderIdx.ID(folder))
	return k
}

func (db *Instance) mtimesKey(folder []byte) []byte {
	prefix := make([]byte, 5) // key type + 4 bytes folder idx number
	prefix[0] = KeyTypeVirtualMtime
	binary.BigEndian.PutUint32(prefix[1:], db.folderIdx.ID(folder))
	return prefix
}

// DropDeltaIndexIDs removes all index IDs from the database. This will
// cause a full index transmission on the next connection.
func (db *Instance) DropDeltaIndexIDs() {
	db.dropPrefix([]byte{KeyTypeIndexID})
}

func (db *Instance) dropMtimes(folder []byte) {
	db.dropPrefix(db.mtimesKey(folder))
}

func (db *Instance) dropPrefix(prefix []byte) {
	t := db.newReadWriteTransaction()
	defer t.close()

	dbi := t.NewIterator(util.BytesPrefix(prefix), nil)
	defer dbi.Release()

	for dbi.Next() {
		t.Delete(dbi.Key())
	}
}

func unmarshalTrunc(bs []byte, truncate bool) (FileIntf, error) {
	if truncate {
		var tf FileInfoTruncated
		err := tf.Unmarshal(bs)
		return tf, err
	}

	var tf protocol.FileInfo
	err := tf.Unmarshal(bs)
	return tf, err
}

// A "better" version of leveldb's errors.IsCorrupted.
func leveldbIsCorrupted(err error) bool {
	switch {
	case err == nil:
		return false

	case errors.IsCorrupted(err):
		return true

	case strings.Contains(err.Error(), "corrupted"):
		return true
	}

	return false
}

// A smallIndex is an in memory bidirectional []byte to uint32 map. It gives
// fast lookups in both directions and persists to the database. Don't use for
// storing more items than fit comfortably in RAM.
type smallIndex struct {
	db     *Instance
	prefix []byte
	id2val map[uint32]string
	val2id map[string]uint32
	nextID uint32
	mut    sync.Mutex
}

func newSmallIndex(db *Instance, prefix []byte) *smallIndex {
	idx := &smallIndex{
		db:     db,
		prefix: prefix,
		id2val: make(map[uint32]string),
		val2id: make(map[string]uint32),
		mut:    sync.NewMutex(),
	}
	idx.load()
	return idx
}

// load iterates over the prefix space in the database and populates the in
// memory maps.
func (i *smallIndex) load() {
	tr := i.db.newReadOnlyTransaction()
	it := tr.NewIterator(util.BytesPrefix(i.prefix), nil)
	for it.Next() {
		val := string(it.Value())
		id := binary.BigEndian.Uint32(it.Key()[len(i.prefix):])
		i.id2val[id] = val
		i.val2id[val] = id
		if id >= i.nextID {
			i.nextID = id + 1
		}
	}
	it.Release()
	tr.close()
}

// ID returns the index number for the given byte slice, allocating a new one
// and persisting this to the database if necessary.
func (i *smallIndex) ID(val []byte) uint32 {
	i.mut.Lock()
	// intentionally avoiding defer here as we want this call to be as fast as
	// possible in the general case (folder ID already exists). The map lookup
	// with the conversion of []byte to string is compiler optimized to not
	// copy the []byte, which is why we don't assign it to a temp variable
	// here.
	if id, ok := i.val2id[string(val)]; ok {
		i.mut.Unlock()
		return id
	}

	id := i.nextID
	i.nextID++

	valStr := string(val)
	i.val2id[valStr] = id
	i.id2val[id] = valStr

	key := make([]byte, len(i.prefix)+8) // prefix plus uint32 id
	copy(key, i.prefix)
	binary.BigEndian.PutUint32(key[len(i.prefix):], id)
	i.db.Put(key, val, nil)

	i.mut.Unlock()
	return id
}

// Val returns the value for the given index number, or (nil, false) if there
// is no such index number.
func (i *smallIndex) Val(id uint32) ([]byte, bool) {
	i.mut.Lock()
	val, ok := i.id2val[id]
	i.mut.Unlock()
	if !ok {
		return nil, false
	}

	return []byte(val), true
}
