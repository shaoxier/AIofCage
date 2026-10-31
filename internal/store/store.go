// Package store persists non-secret metadata (providers and sandboxes) as JSON files under the
// user's Application Support directory. Secrets are never written here.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/filelock"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

var ErrNotFound = errors.New("未找到")

type Store struct {
	dir string
	mu  sync.Mutex
}

// DefaultDir is ~/Library/Application Support/AIofCage.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "AIofCage"), nil
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建数据目录：%w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

// LockOperation 供桌面与本机控制进程协调运行期操作；名称经摘要处理，不能形成任意路径。
func (s *Store) LockOperation(ctx context.Context, name string, exclusive bool) (func(), error) {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return filelock.Acquire(ctx, filepath.Join(s.dir, "locks", key+".lock"), exclusive)
}

func (s *Store) lockMetadata() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.LockOperation(ctx, "metadata", true)
}

func (s *Store) Providers() ([]provider.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []provider.Provider
	return items, s.load("providers.json", &items)
}

func (s *Store) Provider(id string) (provider.Provider, error) {
	items, err := s.Providers()
	if err != nil {
		return provider.Provider{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return provider.Provider{}, fmt.Errorf("供应商 %s %w", id, ErrNotFound)
}

// SaveProvider inserts or replaces a provider by ID.
func (s *Store) SaveProvider(item provider.Provider) error {
	return s.update("providers.json", func(raw *[]json.RawMessage) error {
		var items []provider.Provider
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		replaced := false
		for i := range items {
			if items[i].ID == item.ID {
				items[i], replaced = item, true
			}
		}
		if !replaced {
			items = append(items, item)
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
		return encodeAll(items, raw)
	})
}

func (s *Store) DeleteProvider(id string) error {
	return s.update("providers.json", func(raw *[]json.RawMessage) error {
		var items []provider.Provider
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		kept := items[:0]
		for _, item := range items {
			if item.ID != id {
				kept = append(kept, item)
			}
		}
		return encodeAll(kept, raw)
	})
}

func (s *Store) MCPServices() ([]mcp.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []mcp.Service
	return items, s.load("mcp.json", &items)
}

func (s *Store) MCPService(id string) (mcp.Service, error) {
	items, err := s.MCPServices()
	if err != nil {
		return mcp.Service{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return mcp.Service{}, fmt.Errorf("MCP 服务 %s %w", id, ErrNotFound)
}

func (s *Store) SaveMCPService(item mcp.Service) error {
	return s.update("mcp.json", func(raw *[]json.RawMessage) error {
		var items []mcp.Service
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		replaced := false
		for i := range items {
			if items[i].ID == item.ID {
				items[i], replaced = item, true
			}
		}
		if !replaced {
			items = append(items, item)
		}
		return encodeAll(items, raw)
	})
}

// UpdateMCPService changes one stored service under the store lock, so a change based on what
// the store holds now cannot overwrite an edit made in the meantime.
func (s *Store) UpdateMCPService(id string, change func(*mcp.Service) error) error {
	return s.update("mcp.json", func(raw *[]json.RawMessage) error {
		var items []mcp.Service
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		for i := range items {
			if items[i].ID == id {
				if err := change(&items[i]); err != nil {
					return err
				}
				return encodeAll(items, raw)
			}
		}
		return fmt.Errorf("MCP 服务 %s %w", id, ErrNotFound)
	})
}

func (s *Store) DeleteMCPService(id string) error {
	return s.update("mcp.json", func(raw *[]json.RawMessage) error {
		var items []mcp.Service
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		kept := items[:0]
		for _, item := range items {
			if item.ID != id {
				kept = append(kept, item)
			}
		}
		return encodeAll(kept, raw)
	})
}

func (s *Store) Sandboxes() ([]sandbox.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []sandbox.Sandbox
	return items, s.load("sandboxes.json", &items)
}

func (s *Store) Sandbox(idOrName string) (sandbox.Sandbox, error) {
	items, err := s.Sandboxes()
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	for _, item := range items {
		if item.ID == idOrName || item.Name == idOrName {
			return item, nil
		}
	}
	return sandbox.Sandbox{}, fmt.Errorf("沙箱 %s %w", idOrName, ErrNotFound)
}

// SaveSandbox adds or replaces a sandbox. Names are unique and projects may not overlap; both are
// checked under the store's lock, so that two processes cannot create clashing sandboxes at once.
// A sandbox that keeps its project is saved even if that project overlaps one saved before the
// rule; starting such a sandbox is refused instead.
func (s *Store) SaveSandbox(item sandbox.Sandbox) error {
	return s.update("sandboxes.json", func(raw *[]json.RawMessage) error {
		var items []sandbox.Sandbox
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		index := -1
		for i := range items {
			if items[i].ID == item.ID {
				index = i
			} else if items[i].Name == item.Name {
				return fmt.Errorf("已有同名沙箱：%s", item.Name)
			}
		}
		if index < 0 || items[index].ProjectPath != item.ProjectPath {
			if err := sandbox.CheckProjectOverlap(item.ID, item.ProjectPath, items); err != nil {
				return err
			}
		}
		if index < 0 {
			items = append(items, item)
		} else {
			items[index] = item
		}
		return encodeAll(items, raw)
	})
}

// UpdateSandbox applies change to the stored sandbox atomically, so concurrent writers (the usage
// monitor and lifecycle operations) never overwrite each other's fields.
func (s *Store) UpdateSandbox(id string, change func(*sandbox.Sandbox) error) error {
	return s.update("sandboxes.json", func(raw *[]json.RawMessage) error {
		var items []sandbox.Sandbox
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		for i := range items {
			if items[i].ID == id {
				if err := change(&items[i]); err != nil {
					return err
				}
				return encodeAll(items, raw)
			}
		}
		return fmt.Errorf("沙箱 %s %w", id, ErrNotFound)
	})
}

func (s *Store) DeleteSandbox(id string) error {
	return s.update("sandboxes.json", func(raw *[]json.RawMessage) error {
		var items []sandbox.Sandbox
		if err := decodeAll(*raw, &items); err != nil {
			return err
		}
		kept := items[:0]
		for _, item := range items {
			if item.ID != id {
				kept = append(kept, item)
			}
		}
		return encodeAll(kept, raw)
	})
}

// Setting returns a non-secret application setting ("" when unset).
func (s *Store) Setting(key string) string {
	value, _ := s.SettingValue(key)
	return value
}

// SettingValue distinguishes an unset preference from unreadable or damaged settings.
func (s *Store) SettingValue(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := map[string]string{}
	if err := s.load("settings.json", &values); err != nil {
		return "", err
	}
	if values == nil {
		return "", errors.New("settings.json 内容损坏：需要配置对象")
	}
	return values[key], nil
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockMetadata()
	if err != nil {
		return err
	}
	defer unlock()
	values := map[string]string{}
	if err := s.load("settings.json", &values); err != nil {
		return err
	}
	if values == nil {
		return errors.New("settings.json 内容损坏：需要配置对象")
	}
	values[key] = value
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	return s.write("settings.json", data)
}

func (s *Store) load(name string, target any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s 内容损坏：%w", name, err)
	}
	return nil
}

func (s *Store) update(name string, change func(*[]json.RawMessage) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockMetadata()
	if err != nil {
		return err
	}
	defer unlock()
	var raw []json.RawMessage
	if err := s.load(name, &raw); err != nil {
		return err
	}
	if err := change(&raw); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return s.write(name, data)
}

// write replaces a file atomically: write a temporary file, then rename, so a crash never
// leaves a half-written file. Caller holds s.mu.
func (s *Store) write(name string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, name+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, name))
}

func decodeAll[T any](raw []json.RawMessage, items *[]T) error {
	for _, entry := range raw {
		var item T
		if err := json.Unmarshal(entry, &item); err != nil {
			return err
		}
		*items = append(*items, item)
	}
	return nil
}

func encodeAll[T any](items []T, raw *[]json.RawMessage) error {
	*raw = (*raw)[:0]
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		*raw = append(*raw, data)
	}
	return nil
}
