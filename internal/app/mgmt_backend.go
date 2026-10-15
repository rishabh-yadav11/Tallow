package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/rishabh-yadav11/tallow/internal/admin"
	"github.com/rishabh-yadav11/tallow/internal/config"
	"github.com/rishabh-yadav11/tallow/internal/store"
)

func (a *App) keyRefExists(ref string) bool {
	if a.Keystore == nil {
		return false
	}
	for _, existing := range a.Keystore.List() {
		if existing == ref {
			return true
		}
	}
	return false
}

func (a *App) ListKeyRefs() []string {
	if a.Keystore == nil {
		return []string{}
	}
	return a.Keystore.List()
}

func (a *App) SetKeyRef(ref, value string) error {
	if a.Keystore == nil {
		return fmt.Errorf("keystore is not loaded")
	}
	if a.keyRefExists(ref) {
		return fmt.Errorf("keystore ref %q already exists; delete it before re-uploading", ref)
	}
	return a.Keystore.Set(ref, value)
}

func (a *App) DeleteKeyRef(ref string) error {
	if a.Keystore == nil {
		return fmt.Errorf("keystore is not loaded")
	}
	return a.Keystore.Delete(ref)
}

func jsonFieldName(sf reflect.StructField) string {
	if tag := sf.Tag.Get("json"); tag != "" {
		if name := strings.SplitN(tag, ",", 2)[0]; name != "" {
			return name
		}
	}
	return sf.Name
}

func structField(v reflect.Value, name string) (reflect.Value, bool) {
	v = reflect.Indirect(v)
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := range v.NumField() {
		if jsonFieldName(t.Field(i)) == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func (a *App) cfgSnapshot() *config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b, err := json.Marshal(a.cfg)
	if err != nil {
		return a.cfg
	}
	var cp config.Config
	if err := json.Unmarshal(b, &cp); err != nil {
		return a.cfg
	}
	return &cp
}

func (a *App) GetConfigDoc(root string) ([]byte, error) {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	if cfg == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	if root == "" {
		return json.Marshal(cfg)
	}
	parts := strings.Split(root, "/")
	if len(parts) > 2 {
		return nil, fmt.Errorf("config: path is at most section/field, got %q", root)
	}
	v := reflect.ValueOf(cfg)
	sub, ok := structField(v, parts[0])
	if !ok {
		return nil, fmt.Errorf("config: unknown section %q", parts[0])
	}
	if len(parts) == 1 {
		return json.Marshal(sub.Interface())
	}
	fv, ok := structField(sub, parts[1])
	if !ok {
		return nil, fmt.Errorf("config: %q has no field %q", parts[0], parts[1])
	}
	return json.Marshal(fv.Interface())
}

func decodeInto(cfg *config.Config, root string, body []byte) error {
	v := reflect.ValueOf(cfg).Elem()
	if root == "" {
		if err := json.Unmarshal(body, v.Addr().Interface()); err != nil {
			return fmt.Errorf("config: decode: %w", err)
		}
		return nil
	}
	parts := strings.Split(root, "/")
	if len(parts) > 2 {
		return fmt.Errorf("config: path is at most section/field, got %q", root)
	}
	sub, ok := structField(v, parts[0])
	if !ok {
		return fmt.Errorf("config: unknown section %q", parts[0])
	}
	target := reflect.New(sub.Type()).Elem()
	if len(parts) == 1 {
		if err := json.Unmarshal(body, target.Addr().Interface()); err != nil {
			return fmt.Errorf("config: decode section %q: %w", parts[0], err)
		}
		return setStructField(v, parts[0], target)
	}
	fv, ok := structField(sub, parts[1])
	if !ok {
		return fmt.Errorf("config: %q has no field %q", parts[0], parts[1])
	}
	fieldTarget := reflect.New(fv.Type()).Elem()
	if err := json.Unmarshal(body, fieldTarget.Addr().Interface()); err != nil {
		return fmt.Errorf("config: decode field %q.%q: %w", parts[0], parts[1], err)
	}
	return setStructField(sub, parts[1], fieldTarget)
}

func setStructField(dst reflect.Value, name string, src reflect.Value) error {
	fv, ok := structField(dst, name)
	if !ok {
		return fmt.Errorf("config: no field %q", name)
	}
	if !fv.CanSet() {
		return fmt.Errorf("config: field %q is not settable", name)
	}
	fv.Set(src)
	return nil
}

func deleteField(cfg *config.Config, root string) error {
	v := reflect.ValueOf(cfg).Elem()
	parts := strings.Split(root, "/")
	if len(parts) != 2 {
		return fmt.Errorf("config: DELETE needs a section/field path, got %q", root)
	}
	sub, ok := structField(v, parts[0])
	if !ok {
		return fmt.Errorf("config: unknown section %q", parts[0])
	}
	fv, ok := structField(sub, parts[1])
	if !ok {
		return fmt.Errorf("config: %q has no field %q", parts[0], parts[1])
	}
	if !fv.CanSet() {
		return fmt.Errorf("config: field %q is not settable", parts[1])
	}
	fv.Set(reflect.Zero(fv.Type()))
	return nil
}

func (a *App) persistAndReload(cfg *config.Config) error {
	body, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(a.CfgPath); err == nil {
		perm = fi.Mode().Perm()
	}
	dir := filepath.Dir(a.CfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tallow-config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	if _, err := config.Load(tmpName); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("config: rejected: %w", err)
	}
	if err := os.Rename(tmpName, a.CfgPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := a.Reload(); err != nil {
		return fmt.Errorf("config: reload after write: %w", err)
	}
	return nil
}

func (a *App) PutConfigDoc(root string, body []byte) error {
	cfg := a.cfgSnapshot()
	if err := decodeInto(cfg, root, body); err != nil {
		return err
	}
	return a.persistAndReload(cfg)
}

func (a *App) PatchConfigDoc(root string, body []byte) error {
	base := a.cfgSnapshot()
	rootParts := strings.Split(root, "/")
	if len(rootParts) > 2 {
		return fmt.Errorf("config: path is at most section/field, got %q", root)
	}
	if len(rootParts) == 2 {

		return a.PutConfigDoc(root, body)
	}
	var merges map[string]json.RawMessage
	if err := json.Unmarshal(body, &merges); err != nil {
		return fmt.Errorf("config: patch body must be a JSON object: %w", err)
	}
	rv := reflect.ValueOf(base).Elem()
	if root != "" {
		var ok bool
		rv, ok = structField(rv, rootParts[0])
		if !ok {
			return fmt.Errorf("config: unknown section %q", rootParts[0])
		}
		if rv.Kind() != reflect.Struct {
			return fmt.Errorf("config: section %q is not a struct and cannot be merged", rootParts[0])
		}
	}
	for name, raw := range merges {
		fv, ok := structField(rv, name)
		if !ok {
			return fmt.Errorf("config: unknown field %q", name)
		}
		fieldTarget := reflect.New(fv.Type()).Elem()
		if err := json.Unmarshal(raw, fieldTarget.Addr().Interface()); err != nil {
			return fmt.Errorf("config: decode field %q: %w", name, err)
		}
		if !fv.CanSet() {
			return fmt.Errorf("config: field %q is not settable", name)
		}
		fv.Set(fieldTarget)
	}
	return a.persistAndReload(base)
}

func (a *App) DeleteConfigDoc(root string) error {
	cfg := a.cfgSnapshot()
	if err := deleteField(cfg, root); err != nil {
		return err
	}
	return a.persistAndReload(cfg)
}

func (a *App) ReplayRequest(id string) ([]byte, error) {
	a.mu.RLock()
	ph := a.proxyHandler
	a.mu.RUnlock()
	if ph == nil {
		return nil, fmt.Errorf("proxy not loaded")
	}
	meta, reqBody, _, err := a.store.RequestByID(id)
	if err != nil {
		if errors.Is(err, store.ErrRawNotCaptured) {
			return nil, admin.ErrReplayUnavailable
		}
		return nil, admin.ErrReplayNotFound
	}
	if len(reqBody) == 0 || len(meta.ID) == 0 {
		return nil, admin.ErrReplayUnavailable
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ph.ServeChat(rec, req)
	return rec.Body.Bytes(), nil
}
