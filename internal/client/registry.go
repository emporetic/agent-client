package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Instance struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func RegistryPath(home string) string { return filepath.Join(home, "registry.json") }

func ListInstances(home string) ([]Instance, error) {
	b, err := os.ReadFile(RegistryPath(home))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Instance
	if err = json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("registry.json must be an array of instances: %w", err)
	}
	return list, nil
}

func addToRegistry(home string, in Instance) error {
	list, err := ListInstances(home)
	if err != nil {
		return err
	}
	for _, item := range list {
		if item.Name == in.Name {
			return errors.New("this machine already has an instance named " + in.Name)
		}
	}
	list = append(list, in)
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(RegistryPath(home)), ".registry-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, RegistryPath(home))
}
