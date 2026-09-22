package clientsdb

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// BaseClientID извлекает базовый идентификатор клиента, отсекая суффикс устройства
// после '#', '@' или '/' (например, "user123#phone" -> "user123").
func BaseClientID(clientID string) string {
	if idx := strings.IndexAny(clientID, "#@/"); idx != -1 {
		return clientID[:idx]
	}
	return clientID
}

// ClientInfo содержит метаданные о клиенте
type ClientInfo struct {
	Comment string `json:"comment,omitempty"`
	// MaxStreams - жёсткий потолок одновременных соединений с сервером для
	// этого клиента, независимо от того, что клиент запросит флагом -n.
	// 0 = без ограничения.
	MaxStreams int `json:"max_streams,omitempty"`
}

// Data структура JSON-файла
type Data struct {
	Clients map[string]ClientInfo `json:"clients"`
}

// DB - база данных клиентов с поддержкой hot-reload
type DB struct {
	mu           sync.RWMutex
	path         string
	data         Data
	lastModified time.Time
	active       map[string]int // текущее число открытых соединений на clientID
}

// New создает новую базу данных и загружает ее из файла
func New(path string) (*DB, error) {
	db := &DB{
		path:   path,
		data:   Data{Clients: make(map[string]ClientInfo)},
		active: make(map[string]int),
	}

	if err := db.load(); err != nil {
		// Если файла нет, это не ошибка, просто пустая база
		if !os.IsNotExist(err) {
			return nil, err
		}
	}

	return db, nil
}

// StartHotReload запускает горутину, которая периодически проверяет файл на изменения
func (db *DB) StartHotReload(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			db.loadIfModified()
		}
	}()
}

// IsAuthorized проверяет, разрешен ли клиент (учитывая возможный суффикс устройства)
func (db *DB) IsAuthorized(clientID string) bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	baseID := BaseClientID(clientID)
	_, ok := db.data.Clients[baseID]
	return ok
}

// Add добавляет или обновляет клиента (и сразу сохраняет на диск)
func (db *DB) Add(clientID, comment string, maxStreams int) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	baseID := BaseClientID(clientID)
	db.data.Clients[baseID] = ClientInfo{Comment: comment, MaxStreams: maxStreams}
	return db.save()
}

// TryAcquireStream резервирует один слот соединения для clientID, если не
// превышен его MaxStreams. Каждый успешный вызов должен быть парным с
// ReleaseStream. clientID должен быть уже авторизован вызывающим.
func (db *DB) TryAcquireStream(clientID string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	baseID := BaseClientID(clientID)
	info, ok := db.data.Clients[baseID]
	if !ok {
		return false
	}
	if info.MaxStreams > 0 && db.active[baseID] >= info.MaxStreams {
		return false
	}
	db.active[baseID]++
	return true
}

// ReleaseStream освобождает слот, занятый TryAcquireStream.
func (db *DB) ReleaseStream(clientID string) {
	db.mu.Lock()
	defer db.mu.Unlock()

	baseID := BaseClientID(clientID)
	if db.active[baseID] > 0 {
		db.active[baseID]--
	}
}

// Remove удаляет клиента
func (db *DB) Remove(clientID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	delete(db.data.Clients, clientID)
	return db.save()
}

// List возвращает всех клиентов
func (db *DB) List() map[string]ClientInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()

	res := make(map[string]ClientInfo)
	for k, v := range db.data.Clients {
		res[k] = v
	}
	return res
}

func (db *DB) load() error {
	stat, err := os.Stat(db.path)
	if err != nil {
		return err
	}

	b, err := os.ReadFile(db.path)
	if err != nil {
		return err
	}

	var d Data
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("failed to parse %s: %w", db.path, err)
	}

	if d.Clients == nil {
		d.Clients = make(map[string]ClientInfo)
	}

	db.data = d
	db.lastModified = stat.ModTime()
	return nil
}

func (db *DB) loadIfModified() {
	stat, err := os.Stat(db.path)
	if err != nil {
		return
	}

	db.mu.RLock()
	modTime := db.lastModified
	db.mu.RUnlock()

	if stat.ModTime().After(modTime) {
		db.mu.Lock()
		_ = db.load()
		db.mu.Unlock()
	}
}

func (db *DB) save() error {
	b, err := json.MarshalIndent(db.data, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := db.path + ".tmp"
	err = os.WriteFile(tmpFile, b, 0o600) // содержит Client ID (auth-токены) - не world-readable
	if err == nil {
		err = os.Rename(tmpFile, db.path)
	}
	if err == nil {
		stat, _ := os.Stat(db.path)
		if stat != nil {
			db.lastModified = stat.ModTime()
		}
	}
	return err
}

// WriteClientID отправляет Client ID (1 байт длины + строка) в соединение
func WriteClientID(conn net.Conn, clientID string) error {
	b := []byte(clientID)
	if len(b) > 255 {
		b = b[:255] // Усекаем до 255 байт
	}
	buf := make([]byte, 1+len(b))
	buf[0] = byte(len(b)) //nolint:gosec // len(b) усечён до ≤255 выше
	copy(buf[1:], b)
	_, err := conn.Write(buf)
	return err
}

// ReadClientID читает Client ID из соединения. Учитывает DTLS record-orientated поведение.
func ReadClientID(conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}

	l := int(buf[0])
	if n < 1+l {
		return "", io.ErrUnexpectedEOF
	}
	return string(buf[1 : 1+l]), nil
}
