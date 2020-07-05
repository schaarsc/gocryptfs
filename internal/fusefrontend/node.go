package fusefrontend

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/syscallcompat"
	"github.com/rfjakob/gocryptfs/internal/tlog"
)

// Node is a file or directory in the filesystem tree
// in a gocryptfs mount.
type Node struct {
	fs.Inode
}

// path returns the relative plaintext path of this node
func (n *Node) path() string {
	return n.Path(n.Root())
}

// rootNode returns the Root Node of the filesystem.
func (n *Node) rootNode() *RootNode {
	return n.Root().Operations().(*RootNode)
}

// prepareAtSyscall returns a (dirfd, cName) pair that can be used
// with the "___at" family of system calls (openat, fstatat, unlinkat...) to
// access the backing encrypted directory.
//
// If you pass a `child` file name, the (dirfd, cName) pair will refer to
// a child of this node.
// If `child` is empty, the (dirfd, cName) pair refers to this node itself.
func (n *Node) prepareAtSyscall(child string) (dirfd int, cName string, errno syscall.Errno) {
	p := n.path()
	if child != "" {
		p = filepath.Join(p, child)
	}
	rn := n.rootNode()
	if rn.isFiltered(p) {
		errno = syscall.EPERM
		return
	}
	dirfd, cName, err := rn.openBackingDir(p)
	if err != nil {
		errno = fs.ToErrno(err)
	}
	return
}

// Lookup - FUSE call for discovering a file.
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (ch *fs.Inode, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Get device number and inode number into `st`
	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	// Get unique inode number
	n.rootNode().inoMap.TranslateStat(st)
	out.Attr.FromStat(st)
	// Create child node
	id := fs.StableAttr{
		Mode: uint32(st.Mode),
		Gen:  1,
		Ino:  st.Ino,
	}
	node := &Node{}
	ch = n.NewInode(ctx, node, id)
	return ch, 0
}

// GetAttr - FUSE call for stat()ing a file.
//
// GetAttr is symlink-safe through use of openBackingDir() and Fstatat().
func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) (errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fs.ToErrno(err)
	}
	n.rootNode().inoMap.TranslateStat(st)
	out.Attr.FromStat(st)
	return 0
}

// Create - FUSE call. Creates a new file.
//
// Symlink-safe through the use of Openat().
func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	var err error
	fd := -1
	// Make sure context is nil if we don't want to preserve the owner
	rn := n.rootNode()
	if !rn.args.PreserveOwner {
		ctx = nil
	}
	newFlags := rn.mangleOpenFlags(flags)
	// Handle long file name
	if !rn.args.PlaintextNames && nametransform.IsLongContent(cName) {
		// Create ".name"
		err = rn.nameTransform.WriteLongNameAt(dirfd, cName, name)
		if err != nil {
			return nil, nil, 0, fs.ToErrno(err)
		}
		// Create content
		fd, err = syscallcompat.OpenatUserCtx(dirfd, cName, newFlags|syscall.O_CREAT|syscall.O_EXCL, mode, ctx)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
		}
	} else {
		// Create content, normal (short) file name
		fd, err = syscallcompat.OpenatUserCtx(dirfd, cName, newFlags|syscall.O_CREAT|syscall.O_EXCL, mode, ctx)
	}
	if err != nil {
		// xfstests generic/488 triggers this
		if err == syscall.EMFILE {
			var lim syscall.Rlimit
			syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
			tlog.Warn.Printf("Create %q: too many open files. Current \"ulimit -n\": %d", cName, lim.Cur)
		}
		return nil, nil, 0, fs.ToErrno(err)
	}

	// Get device number and inode number into `st`
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	// Get unique inode number
	rn.inoMap.TranslateStat(&st)
	out.Attr.FromStat(&st)
	// Create child node
	id := fs.StableAttr{
		Mode: uint32(st.Mode),
		Gen:  1,
		Ino:  st.Ino,
	}
	node := &Node{}
	ch := n.NewInode(ctx, node, id)

	f := os.NewFile(uintptr(fd), cName)
	return ch, NewFile2(f, rn, &st), 0, 0
}

// Unlink - FUSE call. Delete a file.
//
// Symlink-safe through use of Unlinkat().
func (n *Node) Unlink(ctx context.Context, name string) (errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Delete content
	err := syscallcompat.Unlinkat(dirfd, cName, 0)
	if err != nil {
		return fs.ToErrno(err)
	}
	// Delete ".name" file
	if !n.rootNode().args.PlaintextNames && nametransform.IsLongContent(cName) {
		err = nametransform.DeleteLongNameAt(dirfd, cName)
		if err != nil {
			tlog.Warn.Printf("Unlink: could not delete .name file: %v", err)
		}
	}
	return fs.ToErrno(err)
}

// Readlink - FUSE call.
//
// Symlink-safe through openBackingDir() + Readlinkat().
func (n *Node) Readlink(ctx context.Context) (out []byte, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	cTarget, err := syscallcompat.Readlinkat(dirfd, cName)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	rn := n.rootNode()
	if rn.args.PlaintextNames {
		return []byte(cTarget), 0
	}
	// Symlinks are encrypted like file contents (GCM) and base64-encoded
	target, err := rn.decryptSymlinkTarget(cTarget)
	if err != nil {
		tlog.Warn.Printf("Readlink %q: decrypting target failed: %v", cName, err)
		return nil, syscall.EIO
	}
	return []byte(target), 0
}

// Open - FUSE call. Open already-existing file.
//
// Symlink-safe through Openat().
func (n *Node) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	rn := n.rootNode()
	newFlags := rn.mangleOpenFlags(flags)
	// Taking this lock makes sure we don't race openWriteOnlyFile()
	rn.openWriteOnlyLock.RLock()
	defer rn.openWriteOnlyLock.RUnlock()

	// Open backing file
	fd, err := syscallcompat.Openat(dirfd, cName, newFlags, 0)
	// Handle a few specific errors
	if err != nil {
		if err == syscall.EMFILE {
			var lim syscall.Rlimit
			syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
			tlog.Warn.Printf("Open %q: too many open files. Current \"ulimit -n\": %d", cName, lim.Cur)
		}
		if err == syscall.EACCES && (int(flags)&syscall.O_ACCMODE) == syscall.O_WRONLY {
			fd, err = rn.openWriteOnlyFile(dirfd, cName, newFlags)
		}
	}
	// Could not handle the error? Bail out
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}

	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	f := os.NewFile(uintptr(fd), cName)
	fh = NewFile2(f, rn, &st)
	return
}

// Setattr - FUSE call. Called for chmod, truncate, utimens, ...
func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) (errno syscall.Errno) {
	var f2 *File2
	if f != nil {
		f2 = f.(*File2)
	} else {
		f, _, errno := n.Open(ctx, syscall.O_RDWR)
		if errno != 0 {
			return errno
		}
		f2 = f.(*File2)
		defer f2.Release()
	}
	return f2.Setattr(ctx, in, out)
}
