// +build linux

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path"
	"path/filepath"
	"syscall"

	shell "github.com/ipfs/go-ipfs-api"
	"github.com/pkg/errors"
)

type UnixFSEntry struct {
	Name string
	Type int
	Size int64
	Hash string
}

func main() {
	log.SetFlags(0)

	flag.Usage = func() {
		log.Printf("usage: %s [options] source /destination", os.Args[0])
		flag.PrintDefaults()
		os.Exit(2)
	}
	flushDepth := flag.Int("flushDepth", 4, "Flush when finishing directories fewer than this many levels below the source.")

	flag.Parse()

	if *flushDepth < 0 {
		log.Print("flushDepth cannot be negative.")
		flag.Usage()
	}

	if flag.NArg() != 2 {
		flag.Usage()
	}

	src := flag.Arg(0)
	dest := flag.Arg(1)

	if dest == "" || dest[0] != '/' {
		log.Print("destination must begin with a slash.")
		flag.Usage()
	}

	fi, err := os.Stat(src)
	if err != nil {
		log.Println("cannot read source:", err)
		flag.Usage()
	}

	if !fi.IsDir() {
		log.Print("source must be a directory")
		flag.Usage()
	}

	ipfs := shell.NewLocalShell()

	if err := walk(context.TODO(), ipfs, src, dest, *flushDepth); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func walk(ctx context.Context, ipfs *shell.Shell, src, dest string, flushDepth int) error {
	dirs, files, err := readDir(src)
	if err != nil {
		return err
	}

	children := make(map[string]bool)

	for _, dir := range dirs {
		if err := walk(ctx, ipfs, filepath.Join(src, dir), path.Join(dest, dir), flushDepth-1); err != nil {
			return err
		}

		children[dir] = true
	}

	if len(dirs) == 0 && len(files) != 0 {
		if err := ipfs.Request("files/mkdir", dest).Option("flush", false).Option("parents", true).Exec(ctx, nil); err != nil {
			return errors.Wrapf(err, "mkdir -p %q", dest)
		}
	}

	for _, fi := range files {
		name := fi.Name()
		if err := addFile(ctx, ipfs, filepath.Join(src, name), path.Join(dest, name), flushDepth-1); err != nil {
			return err
		}

		children[name] = true
	}

	remakeDir := func() error {
		if err := ipfs.Request("files/rm", dest).Option("flush", false).Option("recursive", true).Exec(ctx, nil); err != nil {
			return errors.Wrapf(err, "rm -r %q", dest)
		}
		if err := ipfs.Request("files/mkdir", dest).Option("flush", false).Option("parents", true).Exec(ctx, nil); err != nil {
			return errors.Wrapf(err, "mkdir -p %q", dest)
		}
		return nil
	}

	if len(dirs) == 0 && len(files) == 0 {
		if err := remakeDir(); err != nil {
			return err
		}
	} else {
		var existingFiles struct {
			Entries []UnixFSEntry
		}
		if err := ipfs.Request("files/ls", dest).Option("flush", false).Option("U", true).Exec(ctx, &existingFiles); err != nil {
			return errors.Wrapf(err, "ls %q", dest)
		}

		for _, file := range existingFiles.Entries {
			if !children[file.Name] {
				remotePath := path.Join(dest, file.Name)
				if err := ipfs.Request("files/rm", remotePath).Option("flush", false).Option("recursive", true).Exec(ctx, nil); err != nil {
					return errors.Wrapf(err, "rm -r %q", remotePath)
				}

				log.Println("DELETE", remotePath)
			}
		}
	}

	if flushDepth >= 0 {
		if err := ipfs.Request("files/flush", dest).Exec(ctx, nil); err != nil {
			return errors.Wrapf(err, "flush %q", dest)
		}
		log.Println("FLUSH", dest)
	}

	return nil
}

func readDir(path string) (dirs []string, files []os.FileInfo, err error) {
	f, err := os.Open(path)
	if err != nil {
		return
	}

	infos, err := f.Readdir(0)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return
	}

	for _, fi := range infos {
		if fi.IsDir() {
			dirs = append(dirs, fi.Name())
		} else if fi.Mode().IsRegular() {
			files = append(files, fi)
		}
	}

	return
}

func addFile(ctx context.Context, ipfs *shell.Shell, localPath, remotePath string, flushDepth int) error {
	f, err := os.Open(localPath)
	if err != nil {
		return errors.Wrapf(err, "addFile(%q, %q): open", localPath, remotePath)
	}
	// f will be closed by ipfs.Add

	hash, err := ipfs.Add(f, shell.Pin(false))
	if err != nil {
		return errors.Wrapf(err, "addFile(%q, %q): write", localPath, remotePath)
	}

	var originalHash [256]byte
	if sz, err := syscall.Getxattr(localPath, "user.ipfs-hash", originalHash[:]); err == nil {
		if sz == len(hash) && string(originalHash[:sz]) == hash {
			log.Println("SAME", remotePath)
			return nil
		}
	}

	if err := ipfs.Request("files/rm", remotePath).Option("flush", false).Option("recursive", true).Exec(ctx, nil); err != nil {
		_ = err // TODO: do we need to handle this?
	}

	if err := ipfs.Request("files/cp", "/ipfs/"+hash, remotePath).Option("flush", flushDepth >= 0).Exec(ctx, nil); err != nil {
		return errors.Wrapf(err, "addFile(%q, %q): copy", localPath, remotePath)
	}

	log.Println("FILE", remotePath)
	if flushDepth >= 0 {
		log.Println("FLUSH", remotePath)
	}

	return errors.Wrapf(syscall.Setxattr(localPath, "user.ipfs-hash", []byte(hash), 0), "addFile(%q, %q): cache hash", localPath, remotePath)
}
