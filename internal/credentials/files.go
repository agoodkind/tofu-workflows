package credentials

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	strictjson "sigs.k8s.io/json"
)

const directoryName = "tofu-workflows-credentials"

type ownedPath struct {
	Path      string `json:"path"`
	Identity  string `json:"identity"`
	Directory bool   `json:"directory"`
}

type fileStorage struct {
	root    *os.Root
	paths   []string
	receipt string
	owned   []ownedPath
}

func openRunner() (*os.Root, error) {
	directory := os.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("RUNNER_TEMP must be a clean absolute directory path")
	}

	for path := directory; path != string(filepath.Separator); path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			slog.Error("Runner directory inspection failed", "err", err)

			return nil, fmt.Errorf("inspect RUNNER_TEMP ancestor: %w", err)
		}

		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New(
				"RUNNER_TEMP ancestors must be directories without symbolic links",
			)
		}
	}

	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open RUNNER_TEMP: %w", err)
	}

	return root, nil
}

func filePaths(files map[string]string) ([]string, string, error) {
	base := filepath.Join(os.Getenv("RUNNER_TEMP"), directoryName)

	paths := make([]string, 0, len(files))
	for _, path := range files {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path ||
			strings.ContainsAny(path, "\x00\r\n") {
			return nil, "", errors.New("secret file paths must be clean absolute paths")
		}

		relative, err := filepath.Rel(base, path)
		if err != nil || !filepath.IsLocal(relative) || relative == "." {
			return nil, "", errors.New(
				"secret files must be beneath RUNNER_TEMP/tofu-workflows-credentials",
			)
		}

		if strings.HasPrefix(relative, ".ownership-") {
			return nil, "", errors.New(
				"secret file path uses the reserved ownership receipt prefix",
			)
		}

		paths = append(paths, filepath.Join(directoryName, relative))
	}

	slices.Sort(paths)
	seen := make(map[string]bool)

	for _, path := range paths {
		if seen[path] {
			return nil, "", errors.New(
				"secret file destinations must be distinct and cannot be ancestors of other files",
			)
		}

		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if seen[parent] {
				return nil, "", errors.New(
					"secret file destinations must be distinct and cannot be ancestors of other files",
				)
			}
		}

		seen[path] = true
	}

	receipt := filepath.Join(
		directoryName,
		fmt.Sprintf(".ownership-%x.json", sha256.Sum256([]byte(strings.Join(paths, "\x00")))),
	)

	return paths, receipt, nil
}

func prepareFiles(files map[string]string) (*fileStorage, error) {
	paths, receipt, err := filePaths(files)
	if err != nil {
		return nil, err
	}

	storage := &fileStorage{paths: paths, receipt: receipt}
	if len(paths) == 0 {
		return storage, nil
	}

	storage.root, err = openRunner()
	if err != nil {
		slog.Error("Credential storage validation failed", "err", err)

		return nil, err
	}

	for _, path := range append(slices.Clone(paths), receipt) {
		if err := storage.inspectParents(path); err != nil {
			storage.close()

			return nil, err
		}

		if _, err := storage.root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			storage.close()
			if err != nil {
				return nil, fmt.Errorf("inspect credential destination: %w", err)
			}

			return nil, errors.New("credential destination already exists")
		}
	}

	return storage, nil
}

func (storage *fileStorage) close() {
	if storage.root != nil {
		storage.root.Close()
	}
}

func (storage *fileStorage) inspectParents(path string) error {
	for directory := filepath.Dir(path); directory != "."; directory = filepath.Dir(directory) {
		info, err := storage.root.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			slog.Error("Credential directory inspection failed", "err", err)

			return fmt.Errorf("inspect credential directory: %w", err)
		}

		if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
			return errors.New(
				"credential ancestors must be mode-0700 directories without symbolic links",
			)
		}
	}

	return nil
}

func identify(path string, info os.FileInfo) (ownedPath, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ownedPath{}, errors.New(
			"filesystem does not support credential ownership identities",
		)
	}

	return ownedPath{
		Path:      path,
		Identity:  fmt.Sprintf("%d:%d", stat.Dev, stat.Ino),
		Directory: info.IsDir(),
	}, nil
}

func (storage *fileStorage) record(path string, info os.FileInfo) error {
	owned, err := identify(path, info)
	if err != nil {
		return err
	}

	storage.owned = append(storage.owned, owned)

	return nil
}

func (storage *fileStorage) makeParents(path string) error {
	parts := strings.Split(filepath.Dir(path), string(filepath.Separator))
	for index := range parts {
		directory := filepath.Join(parts[:index+1]...)
		if err := storage.root.Mkdir(directory, 0o700); err != nil {
			if !errors.Is(err, os.ErrExist) {
				slog.Error("Credential directory creation failed", "err", err)

				return fmt.Errorf("create credential directory: %w", err)
			}

			if err := storage.inspectParents(filepath.Join(directory, "child")); err != nil {
				return err
			}

			continue
		}

		info, err := storage.root.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect new credential directory: %w", err)
		}

		if err := storage.record(directory, info); err != nil {
			return err
		}
	}

	return nil
}

func (storage *fileStorage) create(path string, data []byte) error {
	if err := storage.makeParents(path); err != nil {
		return err
	}

	file, err := storage.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Error("Credential file creation failed", "err", err)

		return fmt.Errorf("create credential file: %w", err)
	}

	info, statErr := file.Stat()
	if statErr != nil {
		return errors.Join(fmt.Errorf("inspect new credential file: %w", statErr), file.Close())
	}

	if err := storage.record(path, info); err != nil {
		return errors.Join(err, file.Close())
	}

	_, writeErr := file.Write(data)

	return errors.Join(writeErr, file.Close())
}

func (storage *fileStorage) write(values, files map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		relative, err := filepath.Rel(os.Getenv("RUNNER_TEMP"), files[name])
		if err != nil {
			slog.Error("Credential path resolution failed", "err", err)

			return fmt.Errorf("resolve credential file: %w", err)
		}

		if err := storage.create(relative, []byte(values[name])); err != nil {
			return fmt.Errorf("write credential file: %w", err)
		}
	}

	if len(files) == 0 {
		return nil
	}

	data, err := json.Marshal(storage.owned)
	if err != nil {
		return fmt.Errorf("encode credential ownership: %w", err)
	}

	return storage.create(storage.receipt, data)
}

func (storage *fileStorage) rollback() error {
	var result error

	for _, owned := range slices.Backward(storage.owned) {
		if err := storage.removeOwned(owned); err != nil {
			result = errors.Join(result, err)
		}
	}

	return result
}

func (storage *fileStorage) removeOwned(owned ownedPath) error {
	if err := storage.checkOwned(owned); err != nil {
		return err
	}

	if err := storage.root.Remove(owned.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) ||
			(owned.Directory && errors.Is(err, syscall.ENOTEMPTY)) {
			return nil
		}

		slog.Error("Credential removal failed", "err", err)

		return fmt.Errorf("remove owned credential: %w", err)
	}

	return nil
}

func (storage *fileStorage) checkOwned(owned ownedPath) error {
	if err := storage.inspectParents(owned.Path); err != nil {
		return err
	}

	info, err := storage.root.Lstat(owned.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		slog.Error("Credential ownership inspection failed", "err", err)

		return fmt.Errorf("inspect owned credential: %w", err)
	}

	current, err := identify(owned.Path, info)
	if err != nil {
		return err
	}

	if current != owned || (!owned.Directory && !info.Mode().IsRegular()) {
		return errors.New("credential ownership changed; preserve the replacement")
	}

	return nil
}

func openEnvironment(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("GITHUB_ENV is required")
	}

	info, err := os.Lstat(path)
	if err != nil {
		slog.Error("Environment file inspection failed", "err", err)

		return nil, fmt.Errorf("inspect environment file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("environment file must be a regular file without a symbolic link")
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open environment file: %w", err)
	}

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(
			errors.New("environment file changed during installation"),
			err,
			file.Close(),
		)
	}

	return file, nil
}

// Cleanup removes only credentials and empty directories recorded by
// installation.
func Cleanup(files map[string]string) error {
	paths, receipt, err := filePaths(files)
	if err != nil {
		return err
	}

	if len(paths) == 0 {
		return nil
	}

	root, err := openRunner()
	if err != nil {
		return err
	}

	storage := &fileStorage{root: root, paths: paths, receipt: receipt}
	defer storage.close()
	if err := storage.inspectParents(receipt); err != nil {
		return err
	}

	file, err := root.OpenFile(receipt, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		slog.Error("Credential ownership receipt open failed", "err", err)

		return fmt.Errorf("open credential ownership receipt: %w", err)
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.Join(
			errors.New("credential ownership receipt must be a regular mode-0600 file"),
			err,
			file.Close(),
		)
	}

	const maxOwnershipReceiptBytes = 1024 * 1024

	data, readErr := io.ReadAll(io.LimitReader(file, maxOwnershipReceiptBytes))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return fmt.Errorf("read credential ownership receipt: %w", err)
	}

	strictErrors, decodeErr := strictjson.UnmarshalStrict(data, &storage.owned)
	if err := errors.Join(append(strictErrors, decodeErr)...); err != nil {
		return errors.New("credential ownership receipt is invalid")
	}

	if err := storage.validateReceipt(); err != nil {
		return err
	}

	if err := storage.record(receipt, info); err != nil {
		return err
	}

	for _, owned := range storage.owned {
		if err := storage.checkOwned(owned); err != nil {
			return err
		}
	}

	return storage.rollback()
}

func (storage *fileStorage) validateReceipt() error {
	files := make(map[string]bool)
	directories := make(map[string]bool)

	for _, path := range storage.paths {
		files[path] = true
		for directory := filepath.Dir(path); directory != "."; directory = filepath.Dir(directory) {
			directories[directory] = true
		}
	}

	seen := make(map[string]bool)

	for _, owned := range storage.owned {
		if seen[owned.Path] || owned.Identity == "" ||
			(owned.Directory && !directories[owned.Path]) ||
			(!owned.Directory && !files[owned.Path]) {
			return errors.New(
				"credential ownership receipt does not match the protected file bindings",
			)
		}

		seen[owned.Path] = true
		if !owned.Directory {
			delete(files, owned.Path)
		}
	}

	if len(files) != 0 {
		return errors.New("credential ownership receipt omits a protected file binding")
	}

	return nil
}
