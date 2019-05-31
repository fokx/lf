/*
 * LF: Global Fully Replicated Key/Value Store
 * Copyright (C) 2018-2019  ZeroTier, Inc.  https://www.zerotier.com/
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <http://www.gnu.org/licenses/>.
 *
 * --
 *
 * You can be released from the requirements of the license by purchasing
 * a commercial license. Buying such a license is mandatory as soon as you
 * develop commercial closed-source software that incorporates or links
 * directly against ZeroTier software without disclosing the source code
 * of your own application.
 */

package lf

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc64"
	"io/ioutil"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fuse "bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

var crc64ECMATable = crc64.MakeTable(crc64.ECMA)
var fsUsernamePrefixes = [14]string{"lf0000000000000", "lf000000000000", "lf00000000000", "lf0000000000", "lf000000000", "lf00000000", "lf0000000", "lf000000", "lf00000", "lf0000", "lf000", "lf00", "lf0", "lf"}

// fsLfOwnerToUser generates a Unix username and UID from a hash of an owner's public key.
// A cryptographic hash is used instead of CRC64 just to make it a little bit harder to
// intentionally collide these, but uniqueness of these should not be depended upon!
func fsLfOwnerToUser(o []byte) (string, uint32) {
	h := sha256.Sum256(o)
	c64 := binary.BigEndian.Uint64(h[0:8])
	es := strconv.FormatUint(c64, 36)
	uid := uint32(c64 & 0x7fffffff)
	if uid < 65536 {
		uid += 65536
	}
	return (fsUsernamePrefixes[len(es)] + es), uid
}

type fsNodeWithCommit interface {
	commit() error
}

// FS allows the LF to be mounted as a FUSE filesystem.
type FS struct {
	node             *Node
	mountPoint       string
	rootSelectorName []byte
	root             fsDir
	owner            *Owner
	ownerUID         uint32
	authSignature    []byte
	maskingKey       []byte
	passwd           map[string]uint32
	passwdLock       sync.Mutex
	dirty            map[uint64]fsNodeWithCommit
	dirtyLock        sync.Mutex
	fconn            *fuse.Conn
	fconnLock        sync.Mutex
	commitWaitGroup  sync.WaitGroup
}

// fsImpl just hides FUSE methods from everyone outside the LF package.
type fsImpl struct{ FS }

func (impl *fsImpl) Root() (fusefs.Node, error) { return &impl.root, nil }

func (impl *fsImpl) GenerateInode(parentInode uint64, name string) uint64 {
	i := parentInode + crc64.Checksum([]byte(name), crc64ECMATable)
	if i < 1024 { // inodes under 1024 are reserved for special pseudo-files
		i += 1024
	}
	return i
}

func (impl *fsImpl) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	resp.Blocks = 2147483647
	resp.Bfree = 2147483647
	resp.Bavail = 2147483647
	resp.Files = 2147483647
	resp.Ffree = 2147483647
	resp.Bsize = 4096
	resp.Namelen = fsMaxNameLength
	resp.Frsize = 1
	return nil
}

//////////////////////////////////////////////////////////////////////////////

const (
	fsFileTypeNormal  = 0x000 // normal data file
	fsFileTypeDir     = 0x200 // name of a subdirectory, no data
	fsFileTypeLink    = 0x400 // symbolic link
	fsFileTypeDeleted = 0x600 // dead entry (LF itself has no suitable delete semantic, so just mark it as such)

	fsFileTypeMask  = 0x600   // bit mask for file type from mode
	fsMaxNameLength = 511     // max length of the name field in fsFileHeader (9-bit size)
	fsMaxFileSize   = 1048576 // sanity limit to max file size... this would take FOREVER to store with PoW!
)

type fsFileHeader struct {
	mode          uint   // 2-bit type and 9-bit Unix rwxrwxrwx mode (11 bits total)
	largeFileSize uint   // size of large file or 0 if file fits in just this record
	name          []byte // full name of file
}

func (h *fsFileHeader) appendTo(b []byte) []byte {
	var qw [10]byte
	b = append(b, qw[0:binary.PutUvarint(qw[:], uint64(h.mode)|(uint64(len(h.name))<<11)|(uint64(h.largeFileSize)<<20))]...)
	b = append(b, h.name...)
	return b
}

func (h *fsFileHeader) readFrom(b []byte) ([]byte, error) {
	i, n := binary.Uvarint(b) // read header varint that contains mode, name length, has-next-block flag, and optional large file size
	if n <= 0 {
		return nil, ErrInvalidObject
	}
	b = b[n:]
	h.mode = uint(i & 0x7ff)
	h.largeFileSize = uint(i >> 20)
	h.name = make([]byte, uint((i>>11)&fsMaxNameLength))
	if len(b) < len(h.name) {
		return nil, ErrInvalidObject
	}
	copy(h.name, b[0:len(h.name)])
	b = b[len(h.name):]
	return b, nil
}

//////////////////////////////////////////////////////////////////////////////

// NewFS creates and mounts a new virtual filesystem.
func NewFS(n *Node, mountPoint string, rootSelectorName []byte, owner *Owner, authSignature []byte, maskingKey []byte) (*FS, error) {
	os.MkdirAll(mountPoint, 0755)
	mpInfo, err := os.Stat(mountPoint)
	if err != nil || !mpInfo.IsDir() {
		return nil, errors.New("mount point is not a directory (mkdir attempt failed)")
	}

	ownerName, ownerUID := fsLfOwnerToUser(owner.Public)
	fs := &fsImpl{FS: FS{
		node:             n,
		mountPoint:       mountPoint,
		rootSelectorName: rootSelectorName,
		root: fsDir{
			fsFileHeader: fsFileHeader{
				mode: 0777 | fsFileTypeDir,
				name: nil,
			},
			inode:        1,
			ts:           time.Now(),
			uid:          uint32(os.Getuid()),
			gid:          uint32(os.Getgid()),
			selectorName: rootSelectorName,
			keyRange:     [2]Blob{MakeSelectorKey(rootSelectorName, 0), MakeSelectorKey(rootSelectorName, OrdinalMaxValue)},
		},
		owner:         owner,
		ownerUID:      ownerUID,
		authSignature: authSignature,
		maskingKey:    maskingKey,
		passwd:        make(map[string]uint32),
		dirty:         make(map[uint64]fsNodeWithCommit),
	}}
	fs.root.fs = fs
	fs.passwd[ownerName] = ownerUID

	// Include only ASCII printable characters in volume name so as not to cause UI issues (Mac Finder only AFIAK).
	nameEscaped := make([]byte, 0, len(rootSelectorName))
	for _, c := range rootSelectorName {
		if (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c == '-' || c == '.' || c == ',' || c == '!' {
			nameEscaped = append(nameEscaped, c)
		} else {
			nameEscaped = append(nameEscaped, '_')
		}
	}

	//fuse.Debug = func(msg interface{}) { fmt.Printf("%v\n", msg) }

	n.log[LogLevelNormal].Printf("fs: mounting %s at %s with records owned by %s", rootSelectorName, mountPoint, owner.String())
	fuse.Unmount(mountPoint)
	fs.fconn, err = fuse.Mount(
		mountPoint,
		fuse.DaemonTimeout("300"),
		fuse.FSName("lffs"),
		fuse.Subtype("lffs"),
		fuse.VolumeName("lf-"+n.genesisParameters.Name+"-"+string(nameEscaped)),
		fuse.WritebackCache(),
		fuse.LocalVolume(),
		fuse.NoAppleXattr(),
		fuse.NoAppleDouble(),
		fuse.AllowNonEmptyMount(),
		fuse.AllowOther(),
	)
	if err != nil {
		n.log[LogLevelWarning].Printf("fs: FUSE mount failed: %s", err.Error())
		return nil, err
	}

	go func() {
		defer func() {
			e := recover()
			if e != nil {
				n.log[LogLevelWarning].Printf("WARNING: unexpected panic in fs layer: %v", e)
			}
		}()

		<-fs.fconn.Ready

		if fs.fconn.MountError != nil {
			fs.fconnLock.Lock()
			isClosed := fs.fconn == nil
			fs.fconnLock.Unlock()
			if !isClosed {
				n.log[LogLevelWarning].Printf("WARNING: fs: FUSE subsystem failed to enter server mode: %s", fs.fconn.MountError.Error())
			}
		} else {
			n.log[LogLevelNormal].Printf("fs: serving at %s", mountPoint)
			err := fusefs.Serve(fs.fconn, fs)
			fs.fconnLock.Lock()
			isClosed := fs.fconn == nil
			fs.fconnLock.Unlock()
			if err != nil && !isClosed {
				n.log[LogLevelWarning].Printf("WARNING: fs: FUSE subsystem failed to enter server mode: %s", err.Error())
			} else {
				n.log[LogLevelNormal].Printf("fs: unmounted from %s", mountPoint)
			}
			fuse.Unmount(mountPoint)
		}

		fs.fconnLock.Lock()
		if fs.fconn != nil {
			fs.fconn.Close()
			fs.fconn = nil
		}
		fs.fconnLock.Unlock()
	}()

	return &fs.FS, nil
}

// IsOpen returns true if this FS instance is open and serving, false after Close() or if an internal error occurs.
func (fs *FS) IsOpen() bool {
	fs.fconnLock.Lock()
	o := fs.fconn != nil
	fs.fconnLock.Unlock()
	return o
}

// Close unmounts this filesystem
// If a wait group is passed into this function, the goroutine that
// will be spawned to commit dirty records will be added to it and
// then will notify it when done.
func (fs *FS) Close(wg *sync.WaitGroup) error {
	fs.fconnLock.Lock()
	fconn := fs.fconn
	fs.fconn = nil
	fs.fconnLock.Unlock()

	if fconn != nil {
		// The loop accessing .passwd here is a stupid hack to get Linux to actually unmount and close.
		// It has no effect on other architectures.
		var closed uint32
		go func() {
			fconn.Close()
			atomic.StoreUint32(&closed, 1)
		}()
		for atomic.LoadUint32(&closed) == 0 {
			runtime.Gosched()
			ioutil.ReadFile(path.Join(fs.mountPoint, ".passwd"))
			time.Sleep(time.Millisecond * 50)
		}

		fs.dirtyLock.Lock()
		dirty := fs.dirty
		fs.dirty = make(map[uint64]fsNodeWithCommit)
		fs.dirtyLock.Unlock()

		if len(dirty) > 0 {
			if wg != nil {
				wg.Add(1)
			}
			go func() {
				for _, d := range dirty {
					fs.commitWaitGroup.Add(1)
					d.commit()
				}
				if wg != nil {
					wg.Done()
				}
			}()
		}

		fs.commitWaitGroup.Wait()
	}

	return nil
}

//////////////////////////////////////////////////////////////////////////////

// fsDir implements Node and Handle for directories
type fsDir struct {
	fsFileHeader
	inode        uint64    // inode a.k.a. LF ordinal, parent inode + CRC64-ECMA(name)
	ts           time.Time // node timestamp
	uid, gid     uint32    // Unix UID and GID
	selectorName []byte    // name of selector for this directory
	keyRange     [2]Blob   // precomputed (for performance) key range to query all ordinals for entries in this directory
	parent       *fsDir    // parent directory, if any
	fs           *fsImpl   // parent FS instance
}

func (fsn *fsDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Valid = time.Second * 30
	a.Inode = fsn.inode
	a.Size = 0
	a.Blocks = 0
	a.Atime = fsn.ts
	a.Mtime = fsn.ts
	a.Ctime = fsn.ts
	a.Crtime = fsn.ts
	a.Mode = (os.FileMode(fsn.fsFileHeader.mode) & os.ModePerm) | os.ModeDir
	a.Nlink = 1
	a.Uid = fsn.uid
	a.Gid = fsn.gid
	return nil
}

func (fsn *fsDir) Lookup(ctx context.Context, name string) (fusefs.Node, error) {
	if name == ".passwd" {
		if len(fsn.fsFileHeader.name) == 0 {
			var pwdata strings.Builder
			fsn.fs.passwdLock.Lock()
			for o, i := range fsn.fs.passwd {
				pwdata.WriteString(o)
				pwdata.WriteString(":x:")
				pwdata.WriteString(strconv.FormatUint(uint64(i), 10))
				pwdata.WriteRune(':')
				pwdata.WriteString(strconv.FormatUint(uint64(fsn.gid), 10))
				pwdata.WriteString("::")
				pwdata.WriteString(fsn.fs.mountPoint)
				pwdata.WriteString(":/usr/bin/false\n")
			}
			fsn.fs.passwdLock.Unlock()
			return &fsFile{
				fsFileHeader: fsFileHeader{
					mode:          0444 | fsFileTypeNormal,
					largeFileSize: 0,
					name:          []byte(".passwd"),
				},
				inode:     2,
				ts:        time.Now(),
				uid:       fsn.fs.root.uid,
				gid:       fsn.fs.root.gid,
				parent:    fsn,
				data:      []byte(pwdata.String()),
				ephemeral: true,
			}, nil
		}
		return nil, fuse.EPERM
	}

	inode := fsn.fs.GenerateInode(fsn.inode, name)
	maskingKey := fsn.fs.maskingKey
	if len(maskingKey) == 0 {
		maskingKey = fsn.selectorName
	}
	q := Query{
		Range:      []QueryRange{QueryRange{KeyRange: []Blob{MakeSelectorKey(fsn.selectorName, inode)}}},
		MaskingKey: maskingKey,
	}
	qr, _ := q.Execute(fsn.fs.node)

	for _, results := range qr {
		for _, result := range results {
			if len(result.Value) > 0 {
				var f fsFile
				v, err := f.fsFileHeader.readFrom(result.Value)
				if err != nil {
					return nil, fuse.EIO
				}

				if string(f.fsFileHeader.name) == name {
					ownerName, ownerUID := fsLfOwnerToUser(result.Record.Owner)
					fsn.fs.passwdLock.Lock()
					fsn.fs.passwd[ownerName] = ownerUID
					fsn.fs.passwdLock.Unlock()

					switch f.fsFileHeader.mode & fsFileTypeMask {

					case fsFileTypeNormal, fsFileTypeLink:
						var data []byte
						if len(v) > 0 {
							data, err = BrotliDecompress(v, fsMaxFileSize)
							if err != nil {
								return nil, fuse.EIO
							}
						}
						f.inode = inode
						f.ts = time.Unix(int64(result.Record.Timestamp), 0)
						f.uid = ownerUID
						f.gid = fsn.fs.root.gid
						f.parent = fsn
						f.data = data
						return &f, nil

					case fsFileTypeDir:
						var sn []byte
						if fsn.parent != nil {
							sn = append(sn, fsn.parent.selectorName...)
							sn = append(sn, byte('/'))
						}
						sn = append(sn, f.fsFileHeader.name...)
						return &fsDir{
							fsFileHeader: fsFileHeader{
								mode: f.fsFileHeader.mode,
								name: f.fsFileHeader.name,
							},
							inode:        inode,
							ts:           time.Unix(int64(result.Record.Timestamp), 0),
							uid:          ownerUID,
							gid:          fsn.fs.root.gid,
							selectorName: sn,
							keyRange:     [2]Blob{MakeSelectorKey(sn, 0), MakeSelectorKey(sn, OrdinalMaxValue)},
							parent:       fsn,
							fs:           fsn.fs,
						}, nil

					case fsFileTypeDeleted:
						return nil, fuse.ENOENT

					}
				}
			}
		}
	}

	return nil, fuse.ENOENT
}

var one = 1

func (fsn *fsDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	var dir []fuse.Dirent

	if len(fsn.fsFileHeader.name) == 0 {
		dir = append(dir, fuse.Dirent{
			Inode: 2,
			Type:  fuse.DT_File,
			Name:  ".passwd",
		})
	}

	maskingKey := fsn.fs.maskingKey
	if len(maskingKey) == 0 {
		maskingKey = fsn.selectorName
	}
	q := Query{
		Range:      []QueryRange{QueryRange{KeyRange: []Blob{MakeSelectorKey(fsn.selectorName, 0), MakeSelectorKey(fsn.selectorName, OrdinalMaxValue)}}},
		MaskingKey: maskingKey,
		Limit:      &one,
	}
	qr, _ := q.Execute(fsn.fs.node)
	//fmt.Printf("%s\n", PrettyJSON(qr))

	for _, results := range qr {
		for _, result := range results {
			if len(result.Value) > 0 {
				ownerName, ownerUID := fsLfOwnerToUser(result.Record.Owner)
				fsn.fs.passwdLock.Lock()
				fsn.fs.passwd[ownerName] = ownerUID
				fsn.fs.passwdLock.Unlock()

				var fh fsFileHeader
				_, err := fh.readFrom(result.Value)
				if err == nil {
					name := string(fh.name)
					switch fh.mode & fsFileTypeMask {
					case fsFileTypeNormal:
						dir = append(dir, fuse.Dirent{
							Inode: fsn.fs.GenerateInode(fsn.inode, name),
							Type:  fuse.DT_File,
							Name:  name,
						})
					case fsFileTypeDir:
						dir = append(dir, fuse.Dirent{
							Inode: fsn.fs.GenerateInode(fsn.inode, name),
							Type:  fuse.DT_Dir,
							Name:  name,
						})
					case fsFileTypeLink:
						dir = append(dir, fuse.Dirent{
							Inode: fsn.fs.GenerateInode(fsn.inode, name),
							Type:  fuse.DT_Link,
							Name:  name,
						})
					}
					break
				}
			}
		}
	}

	sort.Slice(dir, func(a, b int) bool {
		return strings.Compare(dir[a].Name, dir[b].Name) < 0
	})

	return dir, nil
}

func (fsn *fsDir) internalMkdir(ctx context.Context, name string, mode uint, commitNow bool) (*fsDir, error) {
	if len(name) == 0 || name == "." || name == ".." {
		return nil, fuse.EIO
	}

	exists, _ := fsn.Lookup(ctx, name)
	if exists != nil {
		return nil, fuse.EEXIST
	}

	var sn []byte
	if fsn.parent != nil {
		sn = append(sn, fsn.parent.selectorName...)
		sn = append(sn, byte('/'))
	}
	sn = append(sn, fsn.fsFileHeader.name...)

	d := &fsDir{
		fsFileHeader: fsFileHeader{
			mode: mode | fsFileTypeDir,
			name: []byte(name),
		},
		inode:        fsn.fs.GenerateInode(fsn.inode, name),
		ts:           time.Now(),
		uid:          fsn.fs.ownerUID,
		gid:          fsn.fs.root.gid,
		selectorName: sn,
		keyRange:     [2]Blob{MakeSelectorKey(sn, 0), MakeSelectorKey(sn, OrdinalMaxValue)},
		parent:       fsn,
		fs:           fsn.fs,
	}

	if commitNow {
		fsn.fs.commitWaitGroup.Add(1)
		go d.commit()
	}

	return d, nil
}

func (fsn *fsDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fusefs.Node, error) {
	return fsn.internalMkdir(ctx, req.Name, uint(req.Mode.Perm()), true)
}

func (fsn *fsDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	if req.Name == ".passwd" {
		if len(fsn.fsFileHeader.name) == 0 {
			return fuse.EPERM
		}
		return fuse.ENOENT
	}

	exists, _ := fsn.Lookup(ctx, req.Name)
	if exists == nil {
		return fuse.ENOENT
	}

	f := &fsFile{
		fsFileHeader: fsFileHeader{
			mode:          fsFileTypeDeleted,
			largeFileSize: 0,
			name:          []byte(req.Name),
		},
		inode:  fsn.fs.GenerateInode(fsn.inode, req.Name),
		ts:     time.Now(),
		uid:    fsn.fs.ownerUID,
		gid:    fsn.fs.root.gid,
		parent: fsn,
		data:   nil,
	}

	fsn.fs.commitWaitGroup.Add(1)
	go f.commit()

	return nil
}

func (fsn *fsDir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fusefs.Node) error {
	if newDir == nil {
		return fuse.EIO
	}
	nd, ok := newDir.(*fsDir)
	if !ok {
		return fuse.EIO
	}

	oldNode, err := fsn.Lookup(ctx, req.OldName)
	if err != nil {
		return err
	}
	if oldNode == nil {
		return fuse.ENOENT
	}
	var oldAttr fuse.Attr
	oldNode.Attr(ctx, &oldAttr)

	var cresp fuse.CreateResponse
	newNode, _, err := nd.Create(ctx, &fuse.CreateRequest{
		Header: req.Header,
		Name:   req.NewName,
		Flags:  fuse.OpenFlags(os.O_CREATE | os.O_WRONLY | os.O_TRUNC),
		Mode:   oldAttr.Mode,
	}, &cresp)
	if err != nil {
		return err
	}

	nnf, _ := newNode.(*fsFile)
	if nnf != nil {
		of, _ := oldNode.(*fsFile)
		if of != nil {
			nnf.data = of.data
		}

		fsn.fs.commitWaitGroup.Add(1)
		go nnf.commit()

		fsn.Remove(ctx, &fuse.RemoveRequest{
			Header: req.Header,
			Name:   req.OldName,
			Dir:    false,
		})

		return nil
	}

	nnd, _ := newNode.(*fsDir)
	if nnd != nil {
		fsn.fs.commitWaitGroup.Add(1)
		go nnd.commit()

		fsn.Remove(ctx, &fuse.RemoveRequest{
			Header: req.Header,
			Name:   req.OldName,
			Dir:    true,
		})

		return nil
	}

	return fuse.EIO
}

func (fsn *fsDir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fusefs.Node, error) {
	if len(req.NewName) == 0 || len(req.Target) == 0 {
		return nil, fuse.EIO
	}
	if req.NewName == ".passwd" {
		if len(fsn.fsFileHeader.name) == 0 {
			return nil, fuse.EEXIST
		}
		return nil, fuse.EPERM
	}

	exists, _ := fsn.Lookup(ctx, req.NewName)
	if exists != nil {
		return nil, fuse.EEXIST
	}

	f := &fsFile{
		fsFileHeader: fsFileHeader{
			mode:          0666 | fsFileTypeLink,
			largeFileSize: 0,
			name:          []byte(req.NewName),
		},
		inode:  fsn.fs.GenerateInode(fsn.inode, req.NewName),
		ts:     time.Now(),
		uid:    fsn.fs.ownerUID,
		gid:    fsn.fs.root.gid,
		parent: fsn,
		data:   []byte(req.Target),
	}

	fsn.fs.commitWaitGroup.Add(1)
	go f.commit()

	return f, nil
}

func (fsn *fsDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fusefs.Node, fusefs.Handle, error) {
	if len(req.Name) == 0 || req.Name == "." || req.Name == ".." {
		return nil, nil, fuse.EIO
	}
	if req.Name == ".passwd" {
		if len(fsn.fsFileHeader.name) == 0 {
			return nil, nil, fuse.EEXIST
		}
		return nil, nil, fuse.EPERM
	}

	if req.Mode.IsDir() {
		nn, err := fsn.internalMkdir(ctx, req.Name, uint(req.Mode.Perm()), false)
		return nn, nn, err
	}

	if req.Mode.IsRegular() {
		exists, _ := fsn.Lookup(ctx, req.Name)
		if exists != nil {
			return nil, nil, fuse.EEXIST
		}
		f := &fsFile{
			fsFileHeader: fsFileHeader{
				mode: uint(req.Mode.Perm()) | fsFileTypeNormal,
				name: []byte(req.Name),
			},
			inode:  fsn.fs.GenerateInode(fsn.inode, req.Name),
			ts:     time.Now(),
			uid:    fsn.fs.ownerUID,
			gid:    fsn.fs.root.gid,
			parent: fsn,
			data:   nil,
		}
		fsn.fs.dirtyLock.Lock()
		fsn.fs.dirty[f.inode] = f
		fsn.fs.dirtyLock.Unlock()
		return f, f, nil
	}

	return nil, nil, fuse.ENOTSUP
}

func (fsn *fsDir) commit() error {
	defer fsn.fs.commitWaitGroup.Done()

	if fsn.parent == nil {
		return fuse.EIO
	}

	rdata := make([]byte, 0, len(fsn.fsFileHeader.name)+16)
	rdata = fsn.fsFileHeader.appendTo(rdata)

	links, err := fsn.fs.node.db.getLinks2(fsn.fs.node.genesisParameters.RecordMinLinks)
	if err != nil {
		return err
	}

	var wf *Wharrgarblr
	if !fsn.fs.node.localTest && len(fsn.fs.authSignature) == 0 {
		wf = fsn.fs.node.getWorkFunction()
	}

	rec, err := NewRecord(RecordTypeDatum, rdata, links, fsn.fs.maskingKey, [][]byte{fsn.parent.selectorName}, []uint64{fsn.inode}, fsn.fs.authSignature, TimeSec(), wf, fsn.fs.owner)
	if err != nil {
		return err
	}

	return fsn.fs.node.AddRecord(rec)
}

func (fsn *fsDir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return fuse.ENOTSUP
}

func (fsn *fsDir) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	return nil
}

func (fsn *fsDir) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	fsn.fs.dirtyLock.Lock()
	_, dirty := fsn.fs.dirty[fsn.inode]
	delete(fsn.fs.dirty, fsn.inode)
	fsn.fs.dirtyLock.Unlock()
	if dirty {
		fsn.fs.commitWaitGroup.Add(1)
		go fsn.commit()
	}
	return nil
}

//////////////////////////////////////////////////////////////////////////////

// fsFile implements Node and Handle for regular files and links (for links the data is the link target)
type fsFile struct {
	fsFileHeader
	inode     uint64    // inode a.k.a. ordinal computed from parent inode + CRC64-ECMA(name)
	ts        time.Time // timestamp from LF record
	uid, gid  uint32    // Unix UID/GID
	parent    *fsDir    // parent directory node
	data      []byte    // file's data
	ephemeral bool      // if true this file should not be commited to LF
}

func (fsn *fsFile) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Valid = time.Second * 30
	a.Inode = fsn.inode
	var modeMask os.FileMode
	switch fsn.fsFileHeader.mode & fsFileTypeMask {
	case fsFileTypeNormal:
		a.Size = uint64(len(fsn.data))
		a.Blocks = a.Size / 512
	case fsFileTypeLink:
		modeMask = os.ModeSymlink
		a.Size = uint64(len(fsn.data))
		a.Blocks = a.Size / 512
	default:
		return fuse.ENOENT
	}
	a.Atime = fsn.ts
	a.Mtime = fsn.ts
	a.Ctime = fsn.ts
	a.Crtime = fsn.ts
	a.Mode = os.FileMode(fsn.fsFileHeader.mode&0x1ff) | modeMask
	a.Nlink = 1
	a.Uid = fsn.uid
	a.Gid = fsn.gid
	a.BlockSize = 4096
	return nil
}

func (fsn *fsFile) ReadAll(ctx context.Context) ([]byte, error) {
	if (fsn.fsFileHeader.mode & fsFileTypeMask) == fsFileTypeNormal {
		return fsn.data, nil
	}
	return nil, fuse.EIO
}

func (fsn *fsFile) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	if (fsn.fsFileHeader.mode & fsFileTypeMask) == fsFileTypeLink {
		return string(fsn.data), nil
	}
	return "", fuse.EIO
}

func (fsn *fsFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	data, err := fsn.ReadAll(ctx)
	if err != nil {
		return err
	}
	if req.Offset > int64(len(data)) {
		return fuse.EIO
	}
	data = data[uint(req.Offset):]
	if len(data) > req.Size {
		data = data[0:req.Size]
	}
	resp.Data = data
	return nil
}

func (fsn *fsFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if (fsn.fsFileHeader.mode & fsFileTypeMask) != fsFileTypeNormal {
		return fuse.EIO
	}

	if len(req.Data) == 0 {
		return nil
	}
	if req.Offset >= fsMaxFileSize {
		return fuse.EIO
	}
	eofPos := int(req.Offset + int64(len(req.Data)))
	if eofPos > fsMaxFileSize {
		eofPos = fsMaxFileSize
	}
	if eofPos == int(req.Offset) {
		return nil
	}

	if eofPos > len(fsn.data) {
		d2 := make([]byte, eofPos)
		copy(d2, fsn.data)
		fsn.data = d2
	}
	copy(fsn.data[int(req.Offset):], req.Data[0:eofPos-int(req.Offset)])

	fsn.parent.fs.dirtyLock.Lock()
	fsn.parent.fs.dirty[fsn.inode] = fsn
	fsn.parent.fs.dirtyLock.Unlock()

	resp.Size = len(req.Data)

	return nil
}

func (fsn *fsFile) commit() error {
	defer fsn.parent.fs.commitWaitGroup.Done()

	if fsn.ephemeral || ((fsn.mode&fsFileTypeMask) == fsFileTypeLink && len(fsn.data) == 0) {
		return nil
	}

	var cdata []byte
	if len(fsn.data) > 0 {
		var err error
		cdata, err = BrotliCompress(fsn.data, make([]byte, 0, len(fsn.data)+4))
		if err != nil {
			return err
		}
	}

	rdata := make([]byte, 0, len(cdata)+len(fsn.fsFileHeader.name)+16)
	rdata = fsn.fsFileHeader.appendTo(rdata)
	rdata = append(rdata, cdata...)

	links, err := fsn.parent.fs.node.db.getLinks2(fsn.parent.fs.node.genesisParameters.RecordMinLinks)
	if err != nil {
		return err
	}

	var wf *Wharrgarblr
	if !fsn.parent.fs.node.localTest && len(fsn.parent.fs.authSignature) == 0 {
		wf = fsn.parent.fs.node.getWorkFunction()
	}

	rec, err := NewRecord(RecordTypeDatum, rdata, links, fsn.parent.fs.maskingKey, [][]byte{fsn.parent.selectorName}, []uint64{fsn.inode}, fsn.parent.fs.authSignature, TimeSec(), wf, fsn.parent.fs.owner)
	if err != nil {
		return err
	}

	return fsn.parent.fs.node.AddRecord(rec)
}

func (fsn *fsFile) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return fuse.ENOTSUP
}

func (fsn *fsFile) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	return nil
}

func (fsn *fsFile) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if fsn.ephemeral {
		return nil
	}
	fsn.parent.fs.dirtyLock.Lock()
	_, dirty := fsn.parent.fs.dirty[fsn.inode]
	delete(fsn.parent.fs.dirty, fsn.inode)
	fsn.parent.fs.dirtyLock.Unlock()
	if dirty {
		fsn.parent.fs.commitWaitGroup.Add(1)
		go fsn.commit()
	}
	return nil
}