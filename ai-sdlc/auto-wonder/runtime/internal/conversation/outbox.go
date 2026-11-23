package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type deliveryOutbox struct {
	dir string
}

func newDeliveryOutbox(root string) *deliveryOutbox {
	if root == "" {
		return &deliveryOutbox{}
	}
	return &deliveryOutbox{dir: filepath.Join(root, ".runtime-outbox", "conversations")}
}

func (o *deliveryOutbox) Put(key deliveryKey, value any) error {
	if o == nil || o.dir == "" {
		return nil
	}
	if key.conversationID <= 0 || key.turnID <= 0 {
		return fmt.Errorf("conversation outbox requires positive conversation and turn IDs")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode conversation outbox frame: %w", err)
	}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return fmt.Errorf("create conversation outbox: %w", err)
	}
	temp, err := os.CreateTemp(o.dir, ".frame-*.tmp")
	if err != nil {
		return fmt.Errorf("create conversation outbox frame: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure conversation outbox frame: %w", err)
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write conversation outbox frame: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync conversation outbox frame: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close conversation outbox frame: %w", err)
	}
	if err := os.Rename(tempPath, filepath.Join(o.dir, o.filename(key))); err != nil {
		return fmt.Errorf("publish conversation outbox frame: %w", err)
	}
	cleanup = false
	return nil
}

func (o *deliveryOutbox) Load() (map[deliveryKey]json.RawMessage, error) {
	frames := map[deliveryKey]json.RawMessage{}
	if o == nil || o.dir == "" {
		return frames, nil
	}
	entries, err := os.ReadDir(o.dir)
	if os.IsNotExist(err) {
		return frames, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read conversation outbox: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key, ok := parseDeliveryFilename(entry.Name())
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(o.dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read conversation outbox frame %s: %w", entry.Name(), err)
		}
		frames[key] = json.RawMessage(raw)
	}
	return frames, nil
}

func (o *deliveryOutbox) Delete(key deliveryKey) error {
	if o == nil || o.dir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(o.dir, o.filename(key))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete conversation outbox frame: %w", err)
	}
	return nil
}

func (o *deliveryOutbox) filename(key deliveryKey) string {
	return fmt.Sprintf("c%d-t%d-a%d-s%d-k%d.json",
		key.conversationID, key.turnID, key.attempt, key.seq, key.chunk)
}

func parseDeliveryFilename(name string) (deliveryKey, bool) {
	name = strings.TrimSuffix(name, ".json")
	parts := strings.Split(name, "-")
	if len(parts) != 5 || len(parts[0]) < 2 || len(parts[1]) < 2 || len(parts[2]) < 2 ||
		len(parts[3]) < 2 || len(parts[4]) < 2 ||
		parts[0][0] != 'c' || parts[1][0] != 't' ||
		parts[2][0] != 'a' || parts[3][0] != 's' || parts[4][0] != 'k' {
		return deliveryKey{}, false
	}
	conversationID, err1 := strconv.ParseInt(parts[0][1:], 10, 64)
	turnID, err2 := strconv.ParseInt(parts[1][1:], 10, 64)
	attempt, err3 := strconv.Atoi(parts[2][1:])
	seq, err4 := strconv.ParseInt(parts[3][1:], 10, 64)
	chunk, err5 := strconv.Atoi(parts[4][1:])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return deliveryKey{}, false
	}
	return deliveryKey{
		conversationID: conversationID, turnID: turnID, attempt: attempt, seq: seq, chunk: chunk,
	}, true
}
