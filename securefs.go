package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func ensurePathWithinRoot(root, target string) error {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)

	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil {
		return errors.New("access denied: invalid path")
	}
	rel = strings.TrimSpace(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("access denied: path escapes root")
	}
	return nil
}

func secureStat(root, target string) (os.FileInfo, error) {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return nil, err
	}
	return os.Stat(target)
}

func secureReadFile(root, target string) ([]byte, error) {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

func secureWriteFile(root, target string, data []byte, perm os.FileMode) error {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return err
	}
	return os.WriteFile(target, data, perm)
}

func secureReadDir(root, target string) ([]os.DirEntry, error) {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return nil, err
	}
	return os.ReadDir(target)
}

func secureMkdirAll(root, target string, perm os.FileMode) error {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return err
	}
	return os.MkdirAll(target, perm)
}

func secureOpen(root, target string) (*os.File, error) {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return nil, err
	}
	return os.Open(target)
}

func secureOpenFile(root, target string, flag int, perm os.FileMode) (*os.File, error) {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return nil, err
	}
	return os.OpenFile(target, flag, perm)
}

func secureRename(root, src, dst string) error {
	if err := ensurePathWithinRoot(root, src); err != nil {
		return err
	}
	if err := ensurePathWithinRoot(root, dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func secureRemove(root, target string) error {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return err
	}
	return os.Remove(target)
}

func secureRemoveAll(root, target string) error {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

func secureChmod(root, target string, mode os.FileMode) error {
	if err := ensurePathWithinRoot(root, target); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
