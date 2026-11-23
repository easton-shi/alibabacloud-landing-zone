package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
)

type answerReceipt struct {
	ConversationID  int64     `json:"conversationId"`
	TurnID          int64     `json:"turnId"`
	DispatchAttempt int       `json:"dispatchAttempt"`
	RequestID       string    `json:"requestId"`
	AnswerID        string    `json:"answerId"`
	Status          string    `json:"status"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type answerReceiptStore struct {
	dir string
	mu  sync.Mutex
}

func newAnswerReceiptStore(root string) *answerReceiptStore {
	if root == "" {
		return &answerReceiptStore{}
	}
	return &answerReceiptStore{dir: filepath.Join(root, ".runtime-outbox", "answers")}
}

func (s *answerReceiptStore) Save(frame protocol.ConversationElicitation, status string) error {
	if s == nil || s.dir == "" || frame.AnswerID == "" {
		return nil
	}
	receipt := answerReceipt{
		ConversationID: frame.ConversationID, TurnID: frame.TurnID,
		DispatchAttempt: frame.DispatchAttempt, RequestID: frame.RequestID,
		AnswerID: frame.AnswerID, Status: status, UpdatedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode answer receipt: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create answer receipt directory: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, ".answer-*.tmp")
	if err != nil {
		return fmt.Errorf("create answer receipt: %w", err)
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
		return fmt.Errorf("secure answer receipt: %w", err)
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write answer receipt: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync answer receipt: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close answer receipt: %w", err)
	}
	target := s.path(frame.AnswerID)
	if err := os.Rename(tempPath, target); err != nil {
		if runtime.GOOS != "windows" || !os.IsExist(err) {
			return fmt.Errorf("publish answer receipt: %w", err)
		}
		if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replace answer receipt: %w", removeErr)
		}
		if retryErr := os.Rename(tempPath, target); retryErr != nil {
			return fmt.Errorf("replace answer receipt: %w", retryErr)
		}
	}
	cleanup = false
	return nil
}

func (s *answerReceiptStore) Get(frame protocol.ConversationElicitation) (*answerReceipt, error) {
	if s == nil || s.dir == "" || frame.AnswerID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path(frame.AnswerID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read answer receipt: %w", err)
	}
	var receipt answerReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, fmt.Errorf("decode answer receipt: %w", err)
	}
	if receipt.AnswerID != frame.AnswerID || receipt.ConversationID != frame.ConversationID ||
		receipt.TurnID != frame.TurnID || receipt.DispatchAttempt != frame.DispatchAttempt ||
		receipt.RequestID != frame.RequestID {
		return nil, fmt.Errorf("answer receipt identity mismatch")
	}
	return &receipt, nil
}

func (s *answerReceiptStore) path(answerID string) string {
	digest := sha256.Sum256([]byte(answerID))
	return filepath.Join(s.dir, hex.EncodeToString(digest[:])+".json")
}
