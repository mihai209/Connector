package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func normalizePathWithinRoot(root, target string) (string, error) {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	if !filepath.IsAbs(cleanRoot) {
		absRoot, err := filepath.Abs(cleanRoot)
		if err != nil {
			return "", errors.New("access denied: invalid root")
		}
		cleanRoot = filepath.Clean(absRoot)
	}
	if !filepath.IsAbs(cleanTarget) {
		cleanTarget = filepath.Clean(filepath.Join(cleanRoot, cleanTarget))
	}

	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil {
		return "", errors.New("access denied: invalid path")
	}
	rel = strings.TrimSpace(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("access denied: path escapes root")
	}
	return cleanTarget, nil
}

func secureStat(root, target string) (os.FileInfo, error) {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return nil, err
	}
	return os.Stat(safePath)
}

func secureReadFile(root, target string) ([]byte, error) {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(safePath)
}

func secureWriteFile(root, target string, data []byte, perm os.FileMode) error {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return err
	}
	return os.WriteFile(safePath, data, perm)
}

func secureReadDir(root, target string) ([]os.DirEntry, error) {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(safePath)
}

func secureMkdirAll(root, target string, perm os.FileMode) error {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return err
	}
	return os.MkdirAll(safePath, perm)
}

func secureOpen(root, target string) (*os.File, error) {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return nil, err
	}
	return os.Open(safePath)
}

func secureOpenFile(root, target string, flag int, perm os.FileMode) (*os.File, error) {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(safePath, flag, perm)
}

func secureRename(root, src, dst string) error {
	safeSrc, err := normalizePathWithinRoot(root, src)
	if err != nil {
		return err
	}
	safeDst, err := normalizePathWithinRoot(root, dst)
	if err != nil {
		return err
	}
	return os.Rename(safeSrc, safeDst)
}

func secureRemove(root, target string) error {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return err
	}
	return os.Remove(safePath)
}

func secureRemoveAll(root, target string) error {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return err
	}
	return os.RemoveAll(safePath)
}

func secureChmod(root, target string, mode os.FileMode) error {
	safePath, err := normalizePathWithinRoot(root, target)
	if err != nil {
		return err
	}
	return os.Chmod(safePath, mode)
}
