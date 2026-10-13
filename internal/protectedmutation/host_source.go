package protectedmutation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	HostAuthoritySchemaV1 = 1
	maxHostConfigBytes    = 64 << 10
	maxHostKeyringBytes   = 1 << 20
)

// HostAuthorityConfig is the explicit host-mounted binding for a Beads
// server. Audience, project ID, and database are independent required fields.
type HostAuthorityConfig struct {
	SchemaVersion   int    `json:"schema_version"`
	Audience        string `json:"audience"`
	ProjectID       string `json:"project_id"`
	Database        string `json:"database"`
	KeyringFile     string `json:"keyring_file"`
	RevocationsFile string `json:"revocations_file"`
}

// FileHostAuthoritySource reads one protected host directory containing the
// explicit binding, public keyring, and signed revocation snapshot. The path
// must come from trusted server startup configuration, never request or repo
// data.
type FileHostAuthoritySource struct {
	directory  string
	configName string
	root       *os.Root
	rootInfo   os.FileInfo
	config     HostAuthorityConfig
}

var _ Source = FileHostAuthoritySource{}

// NewFileHostAuthoritySource pins and validates an absolute host-mounted
// authority configuration file. Referenced file names must stay in its
// protected directory.
func NewFileHostAuthoritySource(configPath string) (FileHostAuthoritySource, HostAuthorityConfig, error) {
	if configPath == "" || strings.TrimSpace(configPath) != configPath || !filepath.IsAbs(configPath) {
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, fmt.Errorf("protected mutation authority file path must be absolute: %w", ErrUnavailable)
	}
	configPath = filepath.Clean(configPath)
	directory, configName := filepath.Dir(configPath), filepath.Base(configPath)
	if !validHostFileName(configName) {
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, fmt.Errorf("protected mutation authority file name is invalid: %w", ErrUnavailable)
	}
	info, err := os.Lstat(directory)
	if err != nil || !protectedHostDirectory(info) {
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, fmt.Errorf("protected mutation authority directory is not a protected real directory: %w", ErrUnavailable)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, fmt.Errorf("open protected mutation authority directory: %w", ErrUnavailable)
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !protectedHostDirectory(rootInfo) || !os.SameFile(info, rootInfo) {
		closeErr := root.Close()
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, errors.Join(
			fmt.Errorf("protected mutation authority directory changed while opening: %w", ErrUnavailable), closeErr)
	}
	var config HostAuthorityConfig
	if err := readProtectedHostJSON(root, configName, maxHostConfigBytes, &config); err != nil {
		_ = root.Close()
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, fmt.Errorf("read protected mutation authority config: %w: %v", ErrUnavailable, err)
	}
	if err := validateHostAuthorityConfig(config, configName); err != nil {
		_ = root.Close()
		return FileHostAuthoritySource{}, HostAuthorityConfig{}, err
	}
	source := FileHostAuthoritySource{directory: filepath.Clean(directory), configName: configName, root: root, rootInfo: rootInfo, config: config}
	return source, config, nil
}

// Load rereads the public keyring and current signed snapshot. Missing,
// replaced, writable, symlinked, malformed, or oversized files are errors.
func (s FileHostAuthoritySource) Load(ctx context.Context) (Bundle, error) {
	if s.directory == "" || s.configName == "" || s.root == nil || s.rootInfo == nil {
		return Bundle{}, ErrUnavailable
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
	}
	if err := s.verifyRootIdentity(); err != nil {
		return Bundle{}, err
	}
	var config HostAuthorityConfig
	if err := readProtectedHostJSON(s.root, s.configName, maxHostConfigBytes, &config); err != nil {
		return Bundle{}, fmt.Errorf("read protected mutation authority config: %w: %v", ErrUnavailable, err)
	}
	if err := validateHostAuthorityConfig(config, s.configName); err != nil || config != s.config {
		return Bundle{}, fmt.Errorf("protected mutation authority config changed or is invalid: %w", ErrUnavailable)
	}
	var keyring Keyring
	if err := readProtectedHostJSON(s.root, config.KeyringFile, maxHostKeyringBytes, &keyring); err != nil {
		return Bundle{}, fmt.Errorf("read protected mutation keyring: %w: %v", ErrUnavailable, err)
	}
	if keyring.SchemaVersion != HostAuthoritySchemaV1 || keyring.Generation == 0 {
		return Bundle{}, fmt.Errorf("protected mutation keyring schema or generation is invalid: %w", ErrUnavailable)
	}
	var revocations SignedRevocations
	if err := readProtectedHostJSON(s.root, config.RevocationsFile, maxRevocationBytes, &revocations); err != nil {
		return Bundle{}, fmt.Errorf("read protected mutation revocations: %w: %v", ErrUnavailable, err)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
	}
	return Bundle{Generation: keyring.Generation, Keys: keyring.Keys, Revocations: revocations}, nil
}

// Close releases the pinned host directory handle.
func (s FileHostAuthoritySource) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

func (s FileHostAuthoritySource) verifyRootIdentity() error {
	pathInfo, err := os.Lstat(s.directory)
	if err != nil || !protectedHostDirectory(pathInfo) || !os.SameFile(s.rootInfo, pathInfo) {
		return fmt.Errorf("protected mutation authority root path changed: %w", ErrUnavailable)
	}
	openedInfo, err := s.root.Stat(".")
	if err != nil || !protectedHostDirectory(openedInfo) || !os.SameFile(s.rootInfo, openedInfo) {
		return fmt.Errorf("protected mutation authority root handle changed: %w", ErrUnavailable)
	}
	return nil
}

func validateHostAuthorityConfig(config HostAuthorityConfig, configName string) error {
	if config.SchemaVersion != HostAuthoritySchemaV1 || !validAtom(config.Audience, maxAudienceBytes) ||
		!validAtom(config.ProjectID, 255) || !validAtom(config.Database, 255) ||
		!validHostFileName(config.KeyringFile) || !validHostFileName(config.RevocationsFile) ||
		config.KeyringFile == config.RevocationsFile || config.KeyringFile == configName || config.RevocationsFile == configName {
		return fmt.Errorf("protected mutation authority config is malformed: %w", ErrMalformed)
	}
	return nil
}

func validHostFileName(name string) bool {
	return name != "" && filepath.Base(name) == name && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && validAtom(name, 255)
}

func protectedHostDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0
}

func readProtectedHostJSON(root *os.Root, name string, limit int64, destination any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !protectedHostFile(info, limit) {
		return errors.New("authority file is not a protected regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return closeProtectedHostFile(file, err)
	}
	if !protectedHostFile(openedInfo, limit) || !os.SameFile(info, openedInfo) {
		return closeProtectedHostFile(file, errors.New("authority file changed while opening"))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return closeProtectedHostFile(file, fmt.Errorf("read authority file: %w", err))
	}
	if int64(len(data)) > limit {
		return closeProtectedHostFile(file, errors.New("authority file exceeded size limit"))
	}
	readInfo, err := file.Stat()
	if err != nil {
		return closeProtectedHostFile(file, fmt.Errorf("stat authority file after reading: %w", err))
	}
	if !protectedHostFile(readInfo, limit) || !os.SameFile(openedInfo, readInfo) ||
		openedInfo.Size() != readInfo.Size() || !openedInfo.ModTime().Equal(readInfo.ModTime()) {
		return closeProtectedHostFile(file, errors.New("authority file changed while reading"))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close authority file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return err
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrMalformed
	}
	return nil
}

func closeProtectedHostFile(file *os.File, operationErr error) error {
	return errors.Join(operationErr, file.Close())
}

func protectedHostFile(info os.FileInfo, limit int64) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm()&0o022 == 0 && info.Size() >= 1 && info.Size() <= limit
}
