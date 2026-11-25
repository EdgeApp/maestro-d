package netshape

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
)

// DeviceFields describes an edit. An omitted description keeps its old value.
type DeviceFields struct {
	Description *string
}

// UpsertDevice updates a listed device in place or appends a new one.
func UpsertDevice(path, id string, fields DeviceFields) (Device, error) {
	return editDevice(path, id, &fields)
}

// RemoveDevice removes one entry. A missing device returns os.ErrNotExist.
func RemoveDevice(path, id string) error {
	_, err := editDevice(path, id, nil)
	return err
}

// editableDevice keeps the original shaping bytes, including the spelling
// of numbers, whitespace and escapes inside credentials and request bodies.
type editableDevice struct {
	ID             string          `json:"id"`
	Description    string          `json:"description,omitempty"`
	TrafficShaping json.RawMessage `json:"trafficShaping,omitempty"`
}

func editDevice(path, id string, fields *DeviceFields) (Device, error) {
	id = strings.TrimSpace(id)
	if !utf8.ValidString(id) {
		return Device{}, errors.New("device id must be valid UTF-8")
	}
	if id == "" {
		return Device{}, errors.New("a device id is required")
	}
	path, err := ResolveConfigPath(path)
	if err != nil {
		return Device{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Device{}, err
	}
	lock, err := fsage.LockFile(path + ".lock")
	if err != nil {
		return Device{}, err
	}
	defer fsage.UnlockFile(lock)

	var file struct {
		Devices []editableDevice `json:"hubDevices"`
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return Device{}, err
	default:
		if _, err := ParseConfig(data); err != nil {
			return Device{}, fmt.Errorf("config %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &file); err != nil {
			return Device{}, err
		}
	}
	index := slices.IndexFunc(file.Devices, func(d editableDevice) bool { return strings.TrimSpace(d.ID) == id })
	switch {
	case fields == nil:
		if index < 0 {
			return Device{}, fmt.Errorf("device %q: %w", id, os.ErrNotExist)
		}
		file.Devices = slices.Delete(file.Devices, index, index+1)
	case index < 0:
		index = len(file.Devices)
		file.Devices = append(file.Devices, editableDevice{ID: id})
	}
	if fields != nil && fields.Description != nil {
		file.Devices[index].Description = strings.TrimSpace(*fields.Description)
	}

	data = formatDevices(file.Devices)
	cfg, err := ParseConfig(data)
	if err != nil {
		return Device{}, err
	}
	if err := writeConfigFile(path, data); err != nil {
		return Device{}, err
	}
	return cfg.Devices[id], nil
}

// MarshalIndent would reformat the raw shaping calls and escape their
// strings. Indent the container and copy each validated value verbatim.
func formatDevices(devices []editableDevice) []byte {
	var b bytes.Buffer
	b.WriteString("{\n  \"hubDevices\": [")
	for i, d := range devices {
		if i > 0 {
			b.WriteByte(',')
		}
		id, _ := json.Marshal(d.ID)
		fmt.Fprintf(&b, "\n    {\n      \"id\": %s", id)
		if d.Description != "" {
			description, _ := json.Marshal(d.Description)
			fmt.Fprintf(&b, ",\n      \"description\": %s", description)
		}
		if len(d.TrafficShaping) > 0 {
			b.WriteString(",\n      \"trafficShaping\": ")
			b.Write(d.TrafficShaping)
		}
		b.WriteString("\n    }")
	}
	if len(devices) > 0 {
		b.WriteString("\n  ")
	}
	b.WriteString("]\n}\n")
	return b.Bytes()
}

func writeConfigFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}

// AbsoluteConfigPath makes a config path independent of the caller's working
// directory. On Unix a symlink followed by ".." must be traversed in order.
func AbsoluteConfigPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if runtime.GOOS == "windows" {
		return filepath.Abs(path)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd + string(filepath.Separator) + path, nil
}

// ResolveConfigPath finds the absolute target for both readers and writers,
// including a new config reached through a dangling symbolic link.
func ResolveConfigPath(path string) (string, error) {
	path, err := AbsoluteConfigPath(path)
	if err != nil {
		return "", err
	}
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if !errors.Is(err, os.ErrNotExist) {
			return resolved, err
		}
		// Split preserves ".." in the parent and any relative link target.
		dir, name := filepath.Split(path)
		if name == "" || name == "." || name == ".." {
			return "", err
		}
		target, err := os.Readlink(path)
		if errors.Is(err, os.ErrNotExist) {
			// For a new file, resolve its ancestors before creating it.
			root := len(filepath.VolumeName(dir)) + 1
			for len(dir) > root && os.IsPathSeparator(dir[len(dir)-1]) {
				dir = dir[:len(dir)-1]
			}
			parent, err := ResolveConfigPath(dir)
			if err != nil {
				return "", err
			}
			return filepath.Join(parent, name), nil
		}
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = dir + target
		}
		path = target
	}
}
