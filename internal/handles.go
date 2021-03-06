// Copyright 2015 Ka-Hing Cheung
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

package internal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"

	"github.com/Sirupsen/logrus"
)

type Inode struct {
	Id         fuseops.InodeID
	Name       *string
	FullName   *string
	flags      *FlagStorage
	Attributes *fuseops.InodeAttributes
	AttrTime   time.Time

	log *logHandle

	mu      sync.Mutex          // everything below is protected by mu
	handles map[*DirHandle]bool // value is ignored

	// the refcnt is an exception, it's protected by the global lock
	// Goofys.mu
	refcnt uint64
}

func NewInode(name *string, fullName *string, flags *FlagStorage) (inode *Inode) {
	inode = &Inode{Name: name, FullName: fullName, flags: flags}
	inode.handles = make(map[*DirHandle]bool)
	inode.refcnt = 1
	inode.log = GetLogger(*fullName)
	inode.AttrTime = time.Now()

	if inode.flags.DebugFuse {
		inode.log.Level = logrus.DebugLevel
	}
	return
}

type DirHandle struct {
	inode *Inode

	mu          sync.Mutex // everything below is protected by mu
	Entries     []fuseutil.Dirent
	NameToEntry map[string]fuseops.InodeAttributes // XXX use a smaller struct
	Marker      *string
	BaseOffset  int
}

func NewDirHandle(inode *Inode) (dh *DirHandle) {
	dh = &DirHandle{inode: inode}
	dh.NameToEntry = make(map[string]fuseops.InodeAttributes)
	return
}

type FileHandle struct {
	inode *Inode

	dirty     bool
	writeInit sync.Once
	mpuWG     sync.WaitGroup
	etags     []*string

	mu              sync.Mutex
	mpuId           *string
	nextWriteOffset int64
	lastPartId      int

	poolHandle *BufferPoolHandle
	buf        []byte

	lastWriteError error

	// read
	reader        io.ReadCloser
	readBufOffset int64

	// parallel read
	buffers           []*S3ReadBuffer
	existingReadahead int
	seqReadAmount     uint64
}

func NewFileHandle(in *Inode) *FileHandle {
	fh := &FileHandle{inode: in}
	return fh
}

func (inode *Inode) logFuse(op string, args ...interface{}) {
	fuseLog.Debugln(op, inode.Id, *inode.FullName, args)
}

func (inode *Inode) errFuse(op string, args ...interface{}) {
	fuseLog.Errorln(op, inode.Id, *inode.FullName, args)
}

// LOCKS_REQUIRED(parent.mu)
func (parent *Inode) lookupFromDirHandles(name string) (inode *Inode) {
	parent.mu.Lock()
	defer parent.mu.Unlock()

	for dh := range parent.handles {
		attr, ok := dh.NameToEntry[name]
		if ok {
			fullName := parent.getChildName(name)
			inode = NewInode(&name, &fullName, parent.flags)
			inode.Attributes = &attr
			return
		}
	}

	return
}

func (parent *Inode) LookUp(fs *Goofys, name string) (inode *Inode, err error) {
	parent.logFuse("Inode.LookUp", name)

	inode = parent.lookupFromDirHandles(name)
	if inode != nil {
		return
	}

	inode, err = fs.LookUpInodeMaybeDir(name, parent.getChildName(name))
	if err != nil {
		return nil, err
	}

	return
}

func (parent *Inode) getChildName(name string) string {
	if parent.Id == fuseops.RootInodeID {
		return name
	} else {
		return fmt.Sprintf("%v/%v", *parent.FullName, name)
	}
}

// LOCKS_REQUIRED(fs.mu)
// XXX why did I put lock required? This used to return a resurrect bool
// which no long does anything, need to look into that to see if
// that was legacy
func (inode *Inode) Ref() {
	inode.logFuse("Ref", inode.refcnt)

	if inode.refcnt == 0 {
		fuseLog.Errorln("Ref", inode.Id, *inode.FullName, "refcnt == 0")
		panic("refcnt == 0")
	}

	inode.refcnt++
	return
}

// LOCKS_REQUIRED(fs.mu)
func (inode *Inode) DeRef(n uint64) (stale bool) {
	inode.logFuse("DeRef", n, inode.refcnt)

	if inode.refcnt < n {
		panic(fmt.Sprintf("deref %v from %v", n, inode.refcnt))
	}

	inode.refcnt -= n

	stale = (inode.refcnt == 0)
	return
}

func (parent *Inode) Unlink(fs *Goofys, name string) (err error) {
	parent.logFuse("Unlink", name)

	fullName := parent.getChildName(name)

	params := &s3.DeleteObjectInput{
		Bucket: &fs.bucket,
		Key:    &fullName,
	}

	resp, err := fs.s3.DeleteObject(params)
	if err != nil {
		return mapAwsError(err)
	}

	s3Log.Debug(resp)

	return
}

func (parent *Inode) Create(
	fs *Goofys,
	name string) (inode *Inode, fh *FileHandle) {

	parent.logFuse("Create", name)
	fullName := parent.getChildName(name)

	parent.mu.Lock()
	defer parent.mu.Unlock()

	now := time.Now()
	inode = NewInode(&name, &fullName, parent.flags)
	inode.Attributes = &fuseops.InodeAttributes{
		Size:   0,
		Nlink:  1,
		Mode:   fs.flags.FileMode,
		Atime:  now,
		Mtime:  now,
		Ctime:  now,
		Crtime: now,
		Uid:    fs.flags.Uid,
		Gid:    fs.flags.Gid,
	}

	fh = NewFileHandle(inode)
	fh.poolHandle = fs.bufferPool.NewPoolHandle()
	fh.dirty = true

	return
}

func (parent *Inode) MkDir(
	fs *Goofys,
	name string) (inode *Inode, err error) {

	parent.logFuse("MkDir", name)

	fullName := parent.getChildName(name)

	params := &s3.PutObjectInput{
		Bucket: &fs.bucket,
		Key:    aws.String(fullName + "/"),
		Body:   nil,
	}
	_, err = fs.s3.PutObject(params)
	if err != nil {
		err = mapAwsError(err)
		return
	}

	parent.mu.Lock()
	defer parent.mu.Unlock()

	inode = NewInode(&name, &fullName, parent.flags)
	inode.Attributes = &fs.rootAttrs

	return
}

func isEmptyDir(fs *Goofys, fullName string) (isDir bool, err error) {
	fullName += "/"

	params := &s3.ListObjectsInput{
		Bucket:    &fs.bucket,
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int64(2),
		Prefix:    &fullName,
	}

	resp, err := fs.s3.ListObjects(params)
	if err != nil {
		return false, mapAwsError(err)
	}

	if len(resp.CommonPrefixes) > 0 || len(resp.Contents) > 1 {
		err = fuse.ENOTEMPTY
		isDir = true
		return
	}

	if len(resp.Contents) == 1 {
		isDir = true

		if *resp.Contents[0].Key != fullName {
			err = fuse.ENOTEMPTY
		}
	}

	return
}

func (parent *Inode) RmDir(
	fs *Goofys,
	name string) (err error) {

	parent.logFuse("Rmdir", name)

	fullName := parent.getChildName(name)

	isDir, err := isEmptyDir(fs, fullName)
	if err != nil {
		return
	}
	if !isDir {
		return fuse.ENOENT
	}

	fullName += "/"

	params := &s3.DeleteObjectInput{
		Bucket: &fs.bucket,
		Key:    &fullName,
	}

	_, err = fs.s3.DeleteObject(params)
	if err != nil {
		return mapAwsError(err)
	}

	return
}

func (inode *Inode) GetAttributes(fs *Goofys) (*fuseops.InodeAttributes, error) {
	// XXX refresh attributes
	inode.logFuse("GetAttributes")
	return inode.Attributes, nil
}

func (inode *Inode) OpenFile(fs *Goofys) *FileHandle {
	inode.logFuse("OpenFile")
	return NewFileHandle(inode)
}

func (fh *FileHandle) initWrite(fs *Goofys) {
	fh.writeInit.Do(func() {
		fh.mpuWG.Add(1)
		go fh.initMPU(fs)
	})
}

func (fh *FileHandle) initMPU(fs *Goofys) {
	defer func() {
		fh.mpuWG.Done()
	}()

	params := &s3.CreateMultipartUploadInput{
		Bucket:       &fs.bucket,
		Key:          fh.inode.FullName,
		StorageClass: &fs.flags.StorageClass,
	}

	resp, err := fs.s3.CreateMultipartUpload(params)

	fh.mu.Lock()
	defer fh.mu.Unlock()

	if err != nil {
		fh.lastWriteError = mapAwsError(err)
	}

	s3Log.Debug(resp)

	fh.mpuId = resp.UploadId
	fh.etags = make([]*string, 10000) // at most 10K parts

	return
}

func (fh *FileHandle) mpuPartNoSpawn(fs *Goofys, buf []byte, part int) (err error) {
	fh.inode.logFuse("mpuPartNoSpawn", cap(buf), part)
	if cap(buf) != 0 {
		defer fh.poolHandle.Free(buf)
	}

	if part == 0 || part > 10000 {
		panic(fmt.Sprintf("invalid part number: %v", part))
	}

	params := &s3.UploadPartInput{
		Bucket:     &fs.bucket,
		Key:        fh.inode.FullName,
		PartNumber: aws.Int64(int64(part)),
		UploadId:   fh.mpuId,
		Body:       bytes.NewReader(buf),
	}

	s3Log.Debug(params)

	resp, err := fs.s3.UploadPart(params)
	if err != nil {
		return mapAwsError(err)
	}

	en := &fh.etags[part-1]

	if *en != nil {
		panic(fmt.Sprintf("etags for part %v already set: %v", part, **en))
	}
	*en = resp.ETag
	return
}

func (fh *FileHandle) mpuPart(fs *Goofys, buf []byte, part int) {
	defer func() {
		fh.mpuWG.Done()
	}()

	// maybe wait for CreateMultipartUpload
	if fh.mpuId == nil {
		fh.mpuWG.Wait()
		// initMPU might have errored
		if fh.mpuId == nil {
			return
		}
	}

	err := fh.mpuPartNoSpawn(fs, buf, part)
	if err != nil {
		if fh.lastWriteError == nil {
			fh.lastWriteError = mapAwsError(err)
		}
	}
}

func (fh *FileHandle) waitForCreateMPU(fs *Goofys) (err error) {
	if fh.mpuId == nil {
		fh.mu.Unlock()
		fh.initWrite(fs)
		fh.mpuWG.Wait() // wait for initMPU
		fh.mu.Lock()

		if fh.lastWriteError != nil {
			return fh.lastWriteError
		}
	}

	return
}

func (fh *FileHandle) WriteFile(fs *Goofys, offset int64, data []byte) (err error) {
	fh.inode.logFuse("WriteFile", offset, len(data))

	fh.mu.Lock()
	defer fh.mu.Unlock()

	if fh.lastWriteError != nil {
		return fh.lastWriteError
	}

	if offset != fh.nextWriteOffset {
		fh.inode.errFuse("WriteFile: only sequential writes supported", fh.nextWriteOffset, offset)
		fh.lastWriteError = fuse.EINVAL
		return fh.lastWriteError
	}

	if offset == 0 {
		fh.poolHandle = fs.bufferPool.NewPoolHandle()
		fh.dirty = true
	}

	for {
		if cap(fh.buf) == 0 {
			fh.buf = fh.poolHandle.Request()
		}

		nCopied := fh.poolHandle.Copy(&fh.buf, data)
		fh.nextWriteOffset += int64(nCopied)

		if len(fh.buf) == cap(fh.buf) {
			// we filled this buffer, upload this part
			err = fh.waitForCreateMPU(fs)
			if err != nil {
				return
			}

			fh.lastPartId++
			part := fh.lastPartId
			buf := fh.buf
			fh.buf = nil
			fh.mpuWG.Add(1)

			go fh.mpuPart(fs, buf, part)
		}

		if nCopied == len(data) {
			break
		}

		data = data[nCopied:]
	}

	fh.inode.Attributes.Size = uint64(offset + int64(len(data)))

	return
}

func tryReadAll(r io.ReadCloser, buf []byte) (bytesRead int, err error) {
	toRead := len(buf)
	for toRead > 0 {
		buf := buf[bytesRead : bytesRead+int(toRead)]

		nread, err := r.Read(buf)
		bytesRead += nread
		toRead -= nread

		if err != nil {
			return bytesRead, err
		}
	}

	return
}

func (fh *FileHandle) readFromStream(offset int64, buf []byte) (bytesRead int, err error) {
	if fh.inode.flags.DebugFuse {
		defer func() {
			fh.inode.logFuse("< readFromStream", bytesRead)
		}()
	}

	if fh.reader != nil {
		// try to service read from existing stream
		bytesRead, err = tryReadAll(fh.reader, buf)
		if err == io.EOF {
			fh.reader.Close()
			fh.reader = nil
		}
		return
	}

	return
}

type S3ReadBuffer struct {
	s3     *s3.S3
	offset int64
	size   int
	buf    *Buffer
}

func (b S3ReadBuffer) Init(fs *Goofys, fh *FileHandle, offset int64, size int) *S3ReadBuffer {
	b.s3 = fs.s3
	b.offset = offset
	b.size = size

	mbuf := MBuf{}.Init(fh.poolHandle, uint64(size))
	if mbuf == nil {
		return nil
	}

	b.buf = Buffer{}.Init(mbuf, func() (io.ReadCloser, error) {
		params := &s3.GetObjectInput{
			Bucket: &fs.bucket,
			Key:    fh.inode.FullName,
		}

		bytes := fmt.Sprintf("bytes=%v-%v", offset, offset+int64(size)-1)
		params.Range = &bytes

		resp, err := fs.s3.GetObject(params)
		if err != nil {
			return nil, mapAwsError(err)
		}

		return resp.Body, nil
	})

	return &b
}

func (b *S3ReadBuffer) Read(offset int64, p []byte) (n int, err error) {
	if b.offset == offset {
		n, err = io.ReadFull(b.buf, p)
		if err == io.ErrUnexpectedEOF {
			err = nil
		}
		if err == nil {
			b.offset += int64(n)
			b.size -= n
		}
		if b.size < 0 {
			panic("size < 0")
		}

		return
	} else {
		panic(fmt.Sprintf("not the right buffer, expecting %v got %v, %v left", b.offset, offset, b.size))
		err = errors.New(fmt.Sprintf("not the right buffer, expecting %v got %v", b.offset, offset))
		return
	}
}

func (fh *FileHandle) readFromReadAhead(fs *Goofys, offset int64, buf []byte) (bytesRead int, err error) {
	var nread int
	for len(fh.buffers) != 0 {
		nread, err = fh.buffers[0].Read(offset+int64(bytesRead), buf)
		bytesRead += nread
		if err != nil {
			return
		}

		if fh.buffers[0].size == 0 {
			// we've exhausted the first buffer
			fh.buffers[0].buf.Close()
			fh.buffers = fh.buffers[1:]
		}

		buf = buf[nread:]

		if len(buf) == 0 {
			// we've filled the user buffer
			return
		}
	}

	return
}

func (fh *FileHandle) readAhead(fs *Goofys, offset int64, needAtLeast int) (err error) {
	const MAX_READAHEAD = 100 * 1024 * 1024
	const READAHEAD_CHUNK = 20 * 1024 * 1024

	existingReadahead := 0
	for _, b := range fh.buffers {
		existingReadahead += int(b.size)
	}

	readAheadAmount := MAX_READAHEAD

	for readAheadAmount-existingReadahead >= READAHEAD_CHUNK {
		off := offset + int64(existingReadahead)
		remaining := fh.inode.Attributes.Size - uint64(off)

		// only read up to readahead chunk each time
		size := MinInt(readAheadAmount-existingReadahead, READAHEAD_CHUNK)
		size = int(MinUInt64(uint64(size), remaining))

		if size != 0 {
			fh.inode.logFuse("readahead", off, size, existingReadahead)

			readAheadBuf := S3ReadBuffer{}.Init(fs, fh, int64(off), int(size))
			if readAheadBuf != nil {
				fh.buffers = append(fh.buffers, readAheadBuf)
				existingReadahead += size
			} else {
				if existingReadahead != 0 {
					// don't do more readahead now, but don't fail, cross our
					// fingers that we will be able to allocate the buffers
					// later
					return nil
				} else {
					return syscall.ENOMEM
				}
			}
		}

		if size != READAHEAD_CHUNK {
			// that was the last remaining chunk to readahead
			break
		}
	}

	return nil
}

func (fh *FileHandle) ReadFile(fs *Goofys, offset int64, buf []byte) (bytesRead int, err error) {
	fh.inode.logFuse("ReadFile", offset, len(buf))

	fh.mu.Lock()
	defer fh.mu.Unlock()

	defer func() {
		fh.readBufOffset += int64(bytesRead)
		fh.seqReadAmount += uint64(bytesRead)

		if bytesRead != 0 && err != nil {
			err = nil
		}

		fh.inode.logFuse("< ReadFile", bytesRead)
	}()

	if uint64(offset) >= fh.inode.Attributes.Size {
		// nothing to read
		return
	}

	if fh.poolHandle == nil {
		fh.poolHandle = fs.bufferPool.NewPoolHandle()
	}

	if fh.readBufOffset != offset {
		// XXX out of order read, maybe disable prefetching
		fh.inode.logFuse("out of order read", offset, fh.readBufOffset)

		fh.readBufOffset = offset
		fh.seqReadAmount = 0
		if fh.reader != nil {
			fh.reader.Close()
			fh.reader = nil
		}

		for _, b := range fh.buffers {
			b.buf.Close()
		}
		fh.buffers = nil
	}

	if fh.seqReadAmount >= BUF_SIZE {
		if fh.reader != nil {
			fh.inode.logFuse("cutover to the parallel algorithm")
			fh.reader.Close()
			fh.reader = nil
		}

		err = fh.readAhead(fs, offset, len(buf))
		if err == nil {
			bytesRead, err = fh.readFromReadAhead(fs, offset, buf)
			return
		} else {
			// fall back to read serially
			fh.inode.logFuse("not enough memory, fallback to serial read")
			fh.seqReadAmount = 0
		}
	}

	bytesRead, err = fh.readFileSerial(fs, offset, buf)

	return
}

func (fh *FileHandle) Release() {
	// read buffers
	for _, b := range fh.buffers {
		b.buf.Close()
	}
	fh.buffers = nil

	// write buffers
	if fh.poolHandle != nil {
		if fh.poolHandle.inUseBuffers != 0 {
			if fh.lastWriteError == nil {
				panic("buf not freed but error is nil")
			}

			if fh.buf != nil {
				fh.poolHandle.Free(fh.buf)
			}
			// the other in-flight multipart PUT buffers will be
			// freed when they finish/error out
		}
	}
}

func (fh *FileHandle) readFileSerial(fs *Goofys, offset int64, buf []byte) (bytesRead int, err error) {
	defer func() {
		if bytesRead != 0 && err != nil {
			err = nil
		}
	}()

	if uint64(offset) >= fh.inode.Attributes.Size {
		// nothing to read
		return
	}

	bytesRead, err = fh.readFromStream(offset, buf)
	if err != nil {
		return
	}

	if bytesRead == len(buf) || uint64(offset) == fh.inode.Attributes.Size {
		// nothing more to read
		return
	}

	offset += int64(bytesRead)
	buf = buf[bytesRead:]

	params := &s3.GetObjectInput{
		Bucket: &fs.bucket,
		Key:    fh.inode.FullName,
	}

	if offset != 0 {
		bytes := fmt.Sprintf("bytes=%v-", offset)
		params.Range = &bytes
	}

	resp, err := fs.s3.GetObject(params)
	if err != nil {
		return bytesRead, mapAwsError(err)
	}

	fh.reader = resp.Body

	nread, err := tryReadAll(resp.Body, buf)
	if err == io.EOF {
		fh.reader.Close()
		fh.reader = nil
	}

	bytesRead += nread

	return
}

func (fh *FileHandle) flushSmallFile(fs *Goofys) (err error) {
	buf := fh.buf
	fh.buf = nil

	if cap(buf) != 0 {
		defer fh.poolHandle.Free(buf)
	}

	params := &s3.PutObjectInput{
		Bucket:       &fs.bucket,
		Key:          fh.inode.FullName,
		Body:         bytes.NewReader(buf),
		StorageClass: &fs.flags.StorageClass,
	}

	_, err = fs.s3.PutObject(params)
	if err != nil {
		err = mapAwsError(err)
		fh.mu.Lock()
		fh.lastWriteError = err
		fh.mu.Unlock()
	}
	return
}

func (fh *FileHandle) FlushFile(fs *Goofys) (err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	fh.inode.logFuse("FlushFile")

	if !fh.dirty || fh.lastWriteError != nil {
		err = fh.lastWriteError
		return
	}

	// abort mpu on error
	defer func() {
		if err != nil {
			fh.inode.logFuse("<-- FlushFile", err)
			if fh.mpuId != nil {
				go func() {
					params := &s3.AbortMultipartUploadInput{
						Bucket:   &fs.bucket,
						Key:      fh.inode.FullName,
						UploadId: fh.mpuId,
					}

					fh.mpuId = nil
					resp, _ := fs.s3.AbortMultipartUpload(params)
					s3Log.Debug(resp)
				}()
			}
		} else {
			fh.dirty = false
		}

		fh.writeInit = sync.Once{}
		fh.nextWriteOffset = 0
		fh.lastPartId = 0
	}()

	if fh.lastPartId == 0 {
		return fh.flushSmallFile(fs)
	}

	fh.mpuWG.Wait()

	if fh.lastWriteError != nil {
		return fh.lastWriteError
	}

	if fh.mpuId == nil {
		return
	}

	nParts := fh.lastPartId
	if fh.buf != nil {
		// upload last part
		nParts++
		err = fh.mpuPartNoSpawn(fs, fh.buf, nParts)
		if err != nil {
			return
		}
	}

	parts := make([]*s3.CompletedPart, nParts)
	for i := 0; i < nParts; i++ {
		parts[i] = &s3.CompletedPart{
			ETag:       fh.etags[i],
			PartNumber: aws.Int64(int64(i + 1)),
		}
	}

	params := &s3.CompleteMultipartUploadInput{
		Bucket:   &fs.bucket,
		Key:      fh.inode.FullName,
		UploadId: fh.mpuId,
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: parts,
		},
	}

	s3Log.Debug(params)

	resp, err := fs.s3.CompleteMultipartUpload(params)
	if err != nil {
		return mapAwsError(err)
	}

	s3Log.Debug(resp)
	fh.mpuId = nil

	return
}

func (parent *Inode) Rename(fs *Goofys, from string, newParent *Inode, to string) (err error) {
	parent.logFuse("Rename", from, newParent.getChildName(to))

	fromFullName := parent.getChildName(from)

	// XXX don't hold the lock the entire time
	parent.mu.Lock()
	defer parent.mu.Unlock()

	fromIsDir, err := isEmptyDir(fs, fromFullName)
	if err != nil {
		// we don't support renaming a directory that's not empty
		return
	}

	toFullName := newParent.getChildName(to)

	if parent != newParent {
		newParent.mu.Lock()
		defer newParent.mu.Unlock()
	}

	toIsDir, err := isEmptyDir(fs, toFullName)
	if err != nil {
		return
	}

	if fromIsDir && !toIsDir {
		_, err = fs.s3.HeadObject(&s3.HeadObjectInput{Bucket: &fs.bucket, Key: &toFullName})
		if err == nil {
			return fuse.ENOTDIR
		} else {
			err = mapAwsError(err)
			if err != fuse.ENOENT {
				return
			}
		}
	} else if !fromIsDir && toIsDir {
		return syscall.EISDIR
	}

	size := int64(-1)
	if fromIsDir {
		fromFullName += "/"
		toFullName += "/"
		size = 0
	}

	err = fs.copyObjectMaybeMultipart(size, fromFullName, toFullName)
	if err != nil {
		return err
	}

	delParams := &s3.DeleteObjectInput{
		Bucket: &fs.bucket,
		Key:    &fromFullName,
	}

	_, err = fs.s3.DeleteObject(delParams)
	if err != nil {
		return mapAwsError(err)
	}

	return
}

func (inode *Inode) OpenDir() (dh *DirHandle) {
	inode.logFuse("OpenDir")

	dh = NewDirHandle(inode)

	inode.mu.Lock()
	defer inode.mu.Unlock()

	inode.handles[dh] = true

	return
}

// Dirents, sorted by name.
type sortedDirents []fuseutil.Dirent

func (p sortedDirents) Len() int           { return len(p) }
func (p sortedDirents) Less(i, j int) bool { return p[i].Name < p[j].Name }
func (p sortedDirents) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func makeDirEntry(name string, t fuseutil.DirentType) fuseutil.Dirent {
	return fuseutil.Dirent{Name: name, Type: t, Inode: fuseops.RootInodeID + 1}
}

func (dh *DirHandle) ReadDir(fs *Goofys, offset fuseops.DirOffset) (*fuseutil.Dirent, error) {
	// If the request is for offset zero, we assume that either this is the first
	// call or rewinddir has been called. Reset state.
	if offset == 0 {
		dh.Entries = nil
	}

	if offset == 0 {
		e := makeDirEntry(".", fuseutil.DT_Directory)
		e.Offset = 1
		dh.NameToEntry["."] = fs.rootAttrs
		return &e, nil
	} else if offset == 1 {
		e := makeDirEntry("..", fuseutil.DT_Directory)
		e.Offset = 2
		dh.NameToEntry[".."] = fs.rootAttrs
		return &e, nil
	}

	i := int(offset) - dh.BaseOffset - 2
	if i < 0 {
		panic(fmt.Sprintf("invalid offset %v, base=%v", offset, dh.BaseOffset))
	}

	if i >= len(dh.Entries) {
		if dh.Marker != nil {
			// we need to fetch the next page
			dh.Entries = nil
			dh.BaseOffset += i
			i = 0
		}
	}

	if dh.Entries == nil {
		prefix := *dh.inode.FullName
		if len(prefix) != 0 {
			prefix += "/"
		}

		params := &s3.ListObjectsInput{
			Bucket:    &fs.bucket,
			Delimiter: aws.String("/"),
			Marker:    dh.Marker,
			Prefix:    &prefix,
			//MaxKeys:      aws.Int64(3),
		}

		resp, err := fs.s3.ListObjects(params)
		if err != nil {
			return nil, mapAwsError(err)
		}

		s3Log.Debug(resp)

		dh.Entries = make([]fuseutil.Dirent, 0, len(resp.CommonPrefixes)+len(resp.Contents))

		for _, dir := range resp.CommonPrefixes {
			// strip trailing /
			dirName := (*dir.Prefix)[0 : len(*dir.Prefix)-1]
			// strip previous prefix
			dirName = dirName[len(*params.Prefix):]
			if len(dirName) == 0 {
				continue
			}
			dh.Entries = append(dh.Entries, makeDirEntry(dirName, fuseutil.DT_Directory))
			dh.NameToEntry[dirName] = fs.rootAttrs
		}

		for _, obj := range resp.Contents {
			baseName := (*obj.Key)[len(prefix):]
			if len(baseName) == 0 {
				// this is a directory blob
				continue
			}
			dh.Entries = append(dh.Entries, makeDirEntry(baseName, fuseutil.DT_File))
			dh.NameToEntry[baseName] = fuseops.InodeAttributes{
				Size:   uint64(*obj.Size),
				Nlink:  1,
				Mode:   fs.flags.FileMode,
				Atime:  *obj.LastModified,
				Mtime:  *obj.LastModified,
				Ctime:  *obj.LastModified,
				Crtime: *obj.LastModified,
				Uid:    fs.flags.Uid,
				Gid:    fs.flags.Gid,
			}
		}

		sort.Sort(sortedDirents(dh.Entries))

		// Fix up offset fields.
		for i := 0; i < len(dh.Entries); i++ {
			en := &dh.Entries[i]
			// offset is 1 based, also need to account for "." and ".."
			en.Offset = fuseops.DirOffset(i+dh.BaseOffset) + 1 + 2
		}

		if *resp.IsTruncated {
			dh.Marker = resp.NextMarker
		} else {
			dh.Marker = nil
		}
	}

	if i == len(dh.Entries) {
		// we've reached the end
		return nil, nil
	} else if i > len(dh.Entries) {
		return nil, fuse.EINVAL
	}

	return &dh.Entries[i], nil
}

func (dh *DirHandle) CloseDir() error {
	inode := dh.inode

	inode.mu.Lock()
	defer inode.mu.Unlock()
	delete(inode.handles, dh)

	return nil
}
