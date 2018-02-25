package archiver

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/restic/chunker"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/restic"
)

// SelectFunc returns true for all items that should be included (files and
// dirs). If false is returned, files are ignored and dirs are not even walked.
type SelectFunc func(item string, fi os.FileInfo) bool

// ReportFunc is called for all files in the backup.
type ReportFunc func(item string, fi os.FileInfo, action ReportAction)

// ReportAction describes what the archiver decided to do with a new file.
type ReportAction int

// These constants are the possible report actions
const (
	ReportActionUnknown   = 0
	ReportActionNew       = iota // New file, will be archived as is
	ReportActionUnchanged = iota // File is unchanged, the old content from the previous snapshot is used
)

// NewArchiver saves a directory structure to the repo.
type NewArchiver struct {
	Repo   restic.Repository
	Select SelectFunc
	FS     fs.FS

	Report ReportFunc
}

// Valid returns an error if anything is missing.
func (arch *NewArchiver) Valid() error {
	if arch.Repo == nil {
		return errors.New("repo is not set")
	}

	if arch.Select == nil {
		return errors.New("Select is not set")
	}

	if arch.FS == nil {
		return errors.New("FS is not set")
	}

	return nil
}

// SaveFile chunks a file and saves it to the repository.
func (arch *NewArchiver) SaveFile(ctx context.Context, filename string) (*restic.Node, error) {
	debug.Log("%v", filename)
	f, err := arch.FS.OpenFile(filename, fs.O_RDONLY|fs.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}

	chnker := chunker.New(f, arch.Repo.Config().ChunkerPolynomial)

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "Stat")
	}

	node, err := restic.NodeFromFileInfo(f.Name(), fi)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if node.Type != "file" {
		_ = f.Close()
		return nil, errors.Errorf("node type %q is wrong", node.Type)
	}

	node.Content = []restic.ID{}
	buf := make([]byte, chunker.MinSize)
	for {
		chunk, err := chnker.Next(buf)
		if errors.Cause(err) == io.EOF {
			break
		}
		if err != nil {
			_ = f.Close()
			return nil, err
		}

		// test if the context has ben cancelled, return the error
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}

		id, err := arch.Repo.SaveBlob(ctx, restic.DataBlob, chunk.Data, restic.ID{})
		if err != nil {
			_ = f.Close()
			return nil, err
		}

		// test if the context has ben cancelled, return the error
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}

		node.Content = append(node.Content, id)
		buf = chunk.Data
	}

	err = f.Close()
	if err != nil {
		return nil, err
	}

	return node, nil
}

// loadSubtree tries to load the subtree referenced by node. In case of an error, nil is returned.
func (arch *NewArchiver) loadSubtree(ctx context.Context, node *restic.Node) *restic.Tree {
	if node == nil || node.Type != "dir" || node.Subtree == nil {
		return nil
	}

	tree, err := arch.Repo.LoadTree(ctx, *node.Subtree)
	if err != nil {
		debug.Log("unable to load tree %v: %v", node.Subtree.Str(), err)
		// TODO: handle error
		return nil
	}

	return tree
}

// saveDir stores a directory in the repo and returns the tree.
func (arch *NewArchiver) saveDir(ctx context.Context, prefix string, fi os.FileInfo, dir string, previous *restic.Tree) (*restic.Tree, error) {
	debug.Log("%v %v", prefix, dir)

	f, err := arch.FS.Open(dir)
	if err != nil {
		return nil, errors.Wrap(err, "Open")
	}

	entries, err := f.Readdir(-1)
	if err != nil {
		return nil, errors.Wrap(err, "Readdir")
	}

	err = f.Close()
	if err != nil {
		return nil, errors.Wrap(err, "Close")
	}

	tree := restic.NewTree()
	for _, fi := range entries {
		pathname := filepath.Join(dir, fi.Name())

		abspathname, err := filepath.Abs(pathname)
		if err != nil {
			return nil, err
		}

		if !arch.Select(abspathname, fi) {
			debug.Log("% is excluded", pathname)
			continue
		}

		oldNode := previous.Find(fi.Name())

		var node *restic.Node
		switch {
		case fs.IsRegularFile(fi):
			// use oldNode if the file hasn't changed
			if oldNode != nil && !oldNode.IsNewer(pathname, fi) {
				debug.Log("%v hasn't changed, returning old node", pathname)
				node = oldNode
				err = nil
			} else {
				node, err = arch.SaveFile(ctx, pathname)
			}
		case fi.Mode().IsDir():
			oldSubtree := arch.loadSubtree(ctx, oldNode)
			node, err = arch.SaveDir(ctx, path.Join(prefix, fi.Name()), fi, pathname, oldSubtree)
		default:
			node, err = restic.NodeFromFileInfo(pathname, fi)
		}

		if err != nil {
			return nil, err
		}

		err = tree.Insert(node)
		if err != nil {
			return nil, err
		}
	}

	return tree, nil
}

// SaveDir stores a directory in the repo and returns the node.
func (arch *NewArchiver) SaveDir(ctx context.Context, prefix string, fi os.FileInfo, dir string, previous *restic.Tree) (*restic.Node, error) {
	debug.Log("%v %v", prefix, dir)

	treeNode, err := restic.NodeFromFileInfo(dir, fi)
	if err != nil {
		return nil, err
	}

	tree, err := arch.saveDir(ctx, prefix, fi, dir, previous)
	if err != nil {
		return nil, err
	}

	id, err := arch.Repo.SaveTree(ctx, tree)
	if err != nil {
		return nil, err
	}

	treeNode.Subtree = &id
	return treeNode, nil
}

// SnapshotOptions bundle attributes for a new snapshot.
type SnapshotOptions struct {
	Hostname string
	Time     time.Time
	Tags     []string
	Parent   restic.ID
	Targets  []string
}

// Save saves a target (file or directory) to the repo.
func (arch *NewArchiver) Save(ctx context.Context, prefix, target string, previous *restic.Node) (node *restic.Node, err error) {
	debug.Log("%v target %q, previous %v", prefix, target, previous)
	fi, err := arch.FS.Lstat(target)
	if err != nil {
		return nil, err
	}

	abstarget, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}

	if !arch.Select(abstarget, fi) {
		debug.Log("%v is excluded", target)
		return nil, nil
	}

	switch {
	case fs.IsRegularFile(fi):
		// use previous node if the file hasn't changed
		if previous != nil && !previous.IsNewer(target, fi) {
			debug.Log("%v hasn't changed, returning old node", target)
			return previous, err
		}

		node, err = arch.SaveFile(ctx, target)
	case fi.IsDir():
		oldSubtree := arch.loadSubtree(ctx, previous)
		node, err = arch.SaveDir(ctx, prefix, fi, target, oldSubtree)
	default:
		node, err = restic.NodeFromFileInfo(target, fi)
	}

	return node, err
}

// fileChanged returns true if the file's content has changed since the node
// was created.
func fileChanged(fi os.FileInfo, node *restic.Node) bool {
	if node == nil {
		return true
	}

	// check type change
	if node.Type != "file" {
		return true
	}

	// check modification timestamp
	if !fi.ModTime().Equal(node.ModTime) {
		return true
	}

	// check size
	extFI := fs.ExtendedStat(fi)
	if uint64(fi.Size()) != node.Size || uint64(extFI.Size) != node.Size {
		return true
	}

	// check inode
	if node.Inode != extFI.Inode {
		return true
	}

	return false
}

// SaveArchiveTree stores an ArchiveTree in the repo, returned is the tree.
func (arch *NewArchiver) SaveArchiveTree(ctx context.Context, prefix string, atree *ArchiveTree, previous *restic.Tree) (*restic.Tree, error) {
	debug.Log("%v (%v nodes), parent %v", prefix, len(atree.Nodes), previous)

	tree := restic.NewTree()

	for name, subatree := range atree.Nodes {
		debug.Log("%v save node %v", prefix, name)

		// this is a leaf node
		if subatree.Path != "" {
			node, err := arch.Save(ctx, path.Join(prefix, name), subatree.Path, previous.Find(name))
			if err != nil {
				return nil, err
			}

			if node == nil {
				debug.Log("%v excluded: %v", prefix, name)
				continue
			}

			node.Name = name

			err = tree.Insert(node)
			if err != nil {
				return nil, err
			}

			continue
		}

		oldSubtree := arch.loadSubtree(ctx, previous.Find(name))

		// not a leaf node, archive subtree
		subtree, err := arch.SaveArchiveTree(ctx, path.Join(prefix, name), &subatree, oldSubtree)
		if err != nil {
			return nil, err
		}

		id, err := arch.Repo.SaveTree(ctx, subtree)
		if err != nil {
			return nil, err
		}

		if subatree.FileInfoPath == "" {
			return nil, errors.Errorf("FileInfoPath for %v/%v is empty", prefix, name)
		}

		debug.Log("%v, saved subtree %v as %v", prefix, subtree, id.Str())

		fi, err := arch.FS.Lstat(subatree.FileInfoPath)
		if err != nil {
			return nil, err
		}

		debug.Log("%v, dir node data loaded from %v", prefix, subatree.FileInfoPath)

		node, err := restic.NodeFromFileInfo(subatree.FileInfoPath, fi)
		if err != nil {
			return nil, err
		}

		node.Name = name
		node.Subtree = &id

		err = tree.Insert(node)
		if err != nil {
			return nil, err
		}
	}

	return tree, nil
}

func readdirnames(fs fs.FS, dir string) ([]string, error) {
	f, err := fs.Open(dir)
	if err != nil {
		return nil, err
	}

	entries, err := f.Readdirnames(-1)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	err = f.Close()
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// resolveRelativeTargets replaces targets that only contain relative
// directories ("." or "../../") to the contents of the directory.
func resolveRelativeTargets(fs fs.FS, targets []string) ([]string, error) {
	result := make([]string, 0, len(targets))
	for _, target := range targets {
		pc := pathComponents(target, false)
		if len(pc) > 0 {
			result = append(result, target)
			continue
		}

		debug.Log("replacing %q with readdir(%q)", target, target)
		entries, err := readdirnames(fs, target)
		if err != nil {
			return nil, err
		}

		for _, name := range entries {
			result = append(result, filepath.Join(target, name))
		}
	}

	return result, nil
}

// Options collect attributes for a new snapshot.
type Options struct {
	Tags           []string
	Hostname       string
	Excludes       []string
	Time           time.Time
	ParentSnapshot restic.ID
}

// loadParentTree loads a tree referenced by snapshot id. If id is null, nil is returned.
func (arch *NewArchiver) loadParentTree(ctx context.Context, snapshotID restic.ID) *restic.Tree {
	if snapshotID.IsNull() {
		return nil
	}

	debug.Log("load parent snapshot %v", snapshotID)
	sn, err := restic.LoadSnapshot(ctx, arch.Repo, snapshotID)
	if err != nil {
		debug.Log("unable to load snapshot %v: %v", snapshotID, err)
		return nil
	}

	if sn.Tree == nil {
		debug.Log("snapshot %v has empty tree %v", snapshotID)
		return nil
	}

	debug.Log("load parent tree %v", *sn.Tree)
	tree, err := arch.Repo.LoadTree(ctx, *sn.Tree)
	if err != nil {
		debug.Log("unable to load tree %v: %v", *sn.Tree, err)
		return nil
	}
	return tree
}

// Snapshot saves several targets and returns a snapshot.
func (arch *NewArchiver) Snapshot(ctx context.Context, targets []string, opts Options) (*restic.Snapshot, restic.ID, error) {
	err := arch.Valid()
	if err != nil {
		return nil, restic.ID{}, err
	}

	var cleanTargets []string
	for _, t := range targets {
		cleanTargets = append(cleanTargets, filepath.Clean(t))
	}

	debug.Log("targets before resolving: %v", cleanTargets)

	cleanTargets, err = resolveRelativeTargets(arch.FS, cleanTargets)
	if err != nil {
		return nil, restic.ID{}, err
	}

	debug.Log("targets after resolving: %v", cleanTargets)

	atree, err := NewArchiveTree(cleanTargets)
	if err != nil {
		return nil, restic.ID{}, err
	}

	tree, err := arch.SaveArchiveTree(ctx, "/", atree, arch.loadParentTree(ctx, opts.ParentSnapshot))
	if err != nil {
		return nil, restic.ID{}, err
	}

	rootTreeID, err := arch.Repo.SaveTree(ctx, tree)
	if err != nil {
		return nil, restic.ID{}, err
	}

	err = arch.Repo.Flush(ctx)
	if err != nil {
		return nil, restic.ID{}, err
	}

	err = arch.Repo.SaveIndex(ctx)
	if err != nil {
		return nil, restic.ID{}, err
	}

	sn, err := restic.NewSnapshot(targets, opts.Tags, opts.Hostname, opts.Time)
	sn.Excludes = opts.Excludes
	sn.Tree = &rootTreeID

	id, err := arch.Repo.SaveJSONUnpacked(ctx, restic.SnapshotFile, sn)
	if err != nil {
		return nil, restic.ID{}, err
	}

	return sn, id, nil
}
