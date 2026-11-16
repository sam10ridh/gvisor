// Copyright 2026 The gVisor Authors.
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

package vfs

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/usermem"
)

type copyTestFD struct {
	FileDescriptionDefaultImpl
	DentryMetadataFileDescriptionImpl
	NoLockFD
	fd     FileDescription
	reader io.Reader
	writer io.Writer
}

func newCopyTestFD(t *testing.T, ctx context.Context, vfsObj *VirtualFilesystem, reader io.Reader, writer io.Writer) *FileDescription {
	t.Helper()
	vd := vfsObj.NewAnonVirtualDentry("copy-test")
	defer vd.DecRef(ctx)
	impl := &copyTestFD{reader: reader, writer: writer}
	if err := impl.fd.Init(impl, linux.O_RDWR, auth.CredentialsFromContext(ctx), vd.Mount(), vd.Dentry(), &FileDescriptionOptions{}); err != nil {
		t.Fatalf("FileDescription.Init: %v", err)
	}
	t.Cleanup(func() { impl.fd.DecRef(ctx) })
	return &impl.fd
}

func (*copyTestFD) Release(context.Context) {}

func (fd *copyTestFD) Read(ctx context.Context, dst usermem.IOSequence, _ ReadOptions) (int64, error) {
	buf := make([]byte, dst.NumBytes())
	n, err := fd.reader.Read(buf)
	if copied, copyErr := dst.CopyOut(ctx, buf[:n]); copyErr != nil {
		return int64(copied), copyErr
	}
	return int64(n), err
}

func (fd *copyTestFD) Write(ctx context.Context, src usermem.IOSequence, _ WriteOptions) (int64, error) {
	buf := make([]byte, src.NumBytes())
	if _, err := src.CopyIn(ctx, buf); err != nil {
		return 0, err
	}
	n, err := fd.writer.Write(buf)
	return int64(n), err
}

type copyTestErrorReader struct {
	data []byte
	err  error
}

func (r copyTestErrorReader) Read(dst []byte) (int, error) {
	return copy(dst, r.data), r.err
}

type copyTestWriter struct {
	bytes.Buffer
	maxWrite  int
	failAfter int
	err       error
}

func (w *copyTestWriter) Write(src []byte) (int, error) {
	if w.maxWrite != 0 && len(src) > w.maxWrite {
		src = src[:w.maxWrite]
	}
	var err error
	if w.err != nil && len(src) >= w.failAfter-w.Len() {
		src = src[:w.failAfter-w.Len()]
		err = w.err
	}
	n, _ := w.Buffer.Write(src)
	return n, err
}

func TestCopyRegularFileData(t *testing.T) {
	// Copy a long input before short and empty inputs to check that only bytes
	// belonging to the current source are written to each destination.
	long := bytes.Repeat([]byte("abc123\x00"), 10001)
	for _, test := range []struct {
		name      string
		reader    io.Reader
		maxWrite  int
		failAfter int
		writeErr  error
		want      []byte
		wantErr   error
	}{
		{name: "long", reader: bytes.NewReader(long), want: long},
		{name: "short-after-long", reader: bytes.NewBufferString("x"), want: []byte("x")},
		{name: "empty", reader: bytes.NewReader(nil)},
		{name: "short-reads", reader: iotest.OneByteReader(bytes.NewBufferString("short reads")), want: []byte("short reads")},
		{name: "short-writes", reader: bytes.NewReader(long), maxWrite: 19, want: long},
		{name: "data-with-eof", reader: iotest.DataErrReader(bytes.NewBufferString("last bytes")), want: []byte("last bytes")},
		{name: "read-error", reader: copyTestErrorReader{err: linuxerr.EIO}, wantErr: linuxerr.EIO},
		{name: "data-with-read-error", reader: copyTestErrorReader{data: []byte("discarded"), err: linuxerr.EIO}, wantErr: linuxerr.EIO},
		{name: "read-error-after-data", reader: io.MultiReader(bytes.NewBufferString("prefix"), copyTestErrorReader{err: linuxerr.EIO}), want: []byte("prefix"), wantErr: linuxerr.EIO},
		{name: "write-error", reader: bytes.NewReader(long), writeErr: linuxerr.ENOSPC, wantErr: linuxerr.ENOSPC},
		{name: "partial-write-error", reader: bytes.NewReader(long), maxWrite: 19, failAfter: 37, writeErr: linuxerr.ENOSPC, want: long[:37], wantErr: linuxerr.ENOSPC},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			vfsObj := &VirtualFilesystem{}
			if err := vfsObj.Init(ctx); err != nil {
				t.Fatalf("VFS init: %v", err)
			}
			t.Cleanup(func() { vfsObj.Release(ctx) })
			writer := &copyTestWriter{maxWrite: test.maxWrite, failAfter: test.failAfter, err: test.writeErr}
			src := newCopyTestFD(t, ctx, vfsObj, test.reader, nil)
			dst := newCopyTestFD(t, ctx, vfsObj, nil, writer)
			n, err := CopyRegularFileData(ctx, dst, src)
			if n != int64(len(test.want)) || err != test.wantErr {
				t.Errorf("CopyRegularFileData = (%d, %v), want (%d, %v)", n, err, len(test.want), test.wantErr)
			}
			if !bytes.Equal(writer.Bytes(), test.want) {
				t.Errorf("copied data differs: got %d bytes, want %d bytes", writer.Len(), len(test.want))
			}
		})
	}
}
