package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"julie.ai/internal/knowledge"
)

type Config struct {
	AllowedChatIDs      []int64                     `json:"allowed_chat_ids"`
	AllowedUserIDs      []int64                     `json:"allowed_user_ids"`
	PollTimeoutSeconds  int                         `json:"poll_timeout_seconds"`
	AgentTimeoutSeconds int                         `json:"agent_timeout_seconds"`
	JulieURL            string                      `json:"julie_url"`
	Embeddings          knowledge.EmbeddingConfig   `json:"embeddings"`
	VectorStore         knowledge.VectorStoreConfig `json:"vector_store"`
}

type HistoryStore interface {
	RememberTelegramMessage(context.Context, int64, int64, int64, string, string) error
	SearchTelegramHistory(context.Context, int64, string, int) ([]knowledge.Result, error)
}

type StateRepository interface {
	SessionForUser(context.Context, int64) (string, bool, error)
	SaveSession(context.Context, int64, string) error
	LastUpdateID(context.Context) (int64, error)
	SetLastUpdateID(context.Context, int64) error
	Close() error
}

type Service struct {
	token        string
	chats        map[int64]struct{}
	users        map[int64]struct{}
	pollWait     int
	agentTimeout time.Duration
	julieURL     string
	store        HistoryStore
	state        StateRepository
	http         *http.Client
	agentHTTP    *http.Client
	apiOrigin    string
}

type update struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
}

func NewService(config Config, token string, store HistoryStore, state StateRepository) (*Service, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("Telegram bot token is required")
	}
	if store == nil {
		return nil, fmt.Errorf("Telegram history embeddings and vector store are required")
	}
	if state == nil {
		return nil, fmt.Errorf("Telegram session and update state store is required")
	}
	if config.PollTimeoutSeconds == 0 {
		config.PollTimeoutSeconds = 30
	}
	if config.PollTimeoutSeconds < 1 || config.PollTimeoutSeconds > 50 {
		return nil, fmt.Errorf("poll timeout must be between 1 and 50 seconds")
	}
	if config.AgentTimeoutSeconds == 0 {
		config.AgentTimeoutSeconds = 180
	}
	if config.AgentTimeoutSeconds < 1 || config.AgentTimeoutSeconds > 600 {
		return nil, fmt.Errorf("agent timeout must be between 1 and 600 seconds")
	}
	if config.JulieURL == "" {
		config.JulieURL = "http://julie-julie:8080"
	}
	julieURL, err := url.Parse(config.JulieURL)
	if err != nil || julieURL.Host == "" || (julieURL.Scheme != "http" && julieURL.Scheme != "https") || julieURL.User != nil || julieURL.RawQuery != "" || julieURL.Fragment != "" {
		return nil, fmt.Errorf("invalid Julie API URL")
	}
	service := &Service{
		token:        strings.TrimSpace(token),
		chats:        make(map[int64]struct{}, len(config.AllowedChatIDs)),
		users:        make(map[int64]struct{}, len(config.AllowedUserIDs)),
		pollWait:     config.PollTimeoutSeconds,
		agentTimeout: time.Duration(config.AgentTimeoutSeconds) * time.Second,
		julieURL:     strings.TrimRight(config.JulieURL, "/"),
		store:        store,
		state:        state,
		http:         &http.Client{Timeout: time.Duration(config.PollTimeoutSeconds+10) * time.Second},
		agentHTTP:    &http.Client{Timeout: time.Duration(config.AgentTimeoutSeconds+10) * time.Second},
		apiOrigin:    "https://api.telegram.org",
	}
	for _, id := range config.AllowedChatIDs {
		if id == 0 {
			return nil, fmt.Errorf("allowed chat IDs must not contain zero")
		}
		service.chats[id] = struct{}{}
	}
	for _, id := range config.AllowedUserIDs {
		if id < 1 {
			return nil, fmt.Errorf("allowed user IDs must be positive")
		}
		service.users[id] = struct{}{}
	}
	return service, nil
}

func (service *Service) Poll(ctx context.Context) {
	lastUpdateID, err := service.state.LastUpdateID(ctx)
	if err != nil {
		log.Printf("Telegram update state unavailable: %v", err)
		return
	}
	offset := int64(0)
	if lastUpdateID >= 0 {
		offset = lastUpdateID + 1
	}
	for ctx.Err() == nil {
		updates, err := service.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Telegram polling failed: %v", err)
			if !wait(ctx, 2*time.Second) {
				return
			}
			continue
		}
		for _, item := range updates {
			if item.ID <= lastUpdateID {
				continue
			}
			if err := service.processUpdate(ctx, item); err != nil {
				log.Printf("Telegram update processing failed: %v", err)
				break
			}
			if item.ID > lastUpdateID {
				if err := service.state.SetLastUpdateID(ctx, item.ID); err != nil {
					log.Printf("Telegram update checkpoint failed: %v", err)
					break
				}
				lastUpdateID = item.ID
				offset = item.ID + 1
			}
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (service *Service) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	query := url.Values{}
	query.Set("timeout", fmt.Sprint(service.pollWait))
	query.Set("allowed_updates", `["message"]`)
	if offset > 0 {
		query.Set("offset", fmt.Sprint(offset))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		service.apiOrigin+"/bot"+service.token+"/getUpdates?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Telegram API request")
	}
	var response struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		Result      []update `json:"result"`
	}
	if err := service.call(request, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, fmt.Errorf("Telegram getUpdates failed: %s", response.Description)
	}
	return response.Result, nil
}

func (service *Service) processUpdate(ctx context.Context, item update) error {
	if item.Message == nil || item.Message.From == nil || strings.TrimSpace(item.Message.Text) == "" {
		return nil
	}
	message := item.Message
	if message.Chat.Type != "private" || message.Chat.ID != message.From.ID || !service.allowedIncoming(message.Chat.ID, message.From.ID) {
		return nil
	}
	if err := service.store.RememberTelegramMessage(ctx, message.Chat.ID, message.From.ID, message.ID, "incoming", message.Text); err != nil {
		return err
	}
	turnCtx, cancel := context.WithTimeout(ctx, service.agentTimeout)
	defer cancel()
	sessionID, err := service.getOrCreateSession(turnCtx, message.From.ID)
	if err != nil {
		return fmt.Errorf("get Julie session: %w", err)
	}
	answer, err := service.askJulie(turnCtx, sessionID, message.Text)
	if err != nil {
		return fmt.Errorf("ask Julie: %w", err)
	}
	return service.sendAnswer(turnCtx, message.From.ID, answer)
}

func (service *Service) allowedIncoming(chatID, userID int64) bool {
	if len(service.users) == 0 {
		return false
	}
	if _, ok := service.users[userID]; !ok {
		return false
	}
	if len(service.chats) > 0 {
		if _, ok := service.chats[chatID]; !ok {
			return false
		}
	}
	return true
}

func (service *Service) allowedChat(chatID int64) bool {
	if _, ok := service.users[chatID]; !ok {
		return false
	}
	if len(service.chats) > 0 {
		_, ok := service.chats[chatID]
		return ok
	}
	return true
}

func (service *Service) getOrCreateSession(ctx context.Context, userID int64) (string, error) {
	if sessionID, ok, err := service.state.SessionForUser(ctx, userID); err != nil {
		return "", err
	} else if ok {
		return sessionID, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, service.julieURL+"/sessions", nil)
	if err != nil {
		return "", fmt.Errorf("create Julie session request")
	}
	var response struct {
		ID string `json:"session_id"`
	}
	if err := service.callJulie(request, &response); err != nil {
		return "", err
	}
	if response.ID == "" || len(response.ID) > 128 {
		return "", fmt.Errorf("Julie returned an invalid session ID")
	}
	if err := service.state.SaveSession(ctx, userID, response.ID); err != nil {
		return "", err
	}
	return response.ID, nil
}

func (service *Service) askJulie(ctx context.Context, sessionID, prompt string) (string, error) {
	body, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, service.julieURL+"/sessions/"+url.PathEscape(sessionID)+"/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create Julie message request")
	}
	request.Header.Set("Content-Type", "application/json")
	var response struct {
		Answer string `json:"answer"`
	}
	if err := service.callJulie(request, &response); err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Answer) == "" {
		return "", fmt.Errorf("Julie returned an empty answer")
	}
	return response.Answer, nil
}

func (service *Service) callJulie(request *http.Request, output any) error {
	response, err := service.agentHTTP.Do(request)
	if err != nil {
		return fmt.Errorf("Julie API request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return fmt.Errorf("read Julie API response: %w", err)
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("Julie API response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Julie API returned HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode Julie API response: %w", err)
	}
	return nil
}

func (service *Service) sendAnswer(ctx context.Context, userID int64, answer string) error {
	for _, part := range splitTelegramText(answer, 4096) {
		if _, _, err := service.sendMessage(ctx, userID, part); err != nil {
			return fmt.Errorf("send Julie answer: %w", err)
		}
	}
	return nil
}

func splitTelegramText(text string, maxRunes int) []string {
	runes := []rune(strings.TrimSpace(text))
	parts := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > 0 {
		end := min(len(runes), maxRunes)
		if end < len(runes) {
			for boundary := end; boundary > maxRunes/2; boundary-- {
				if runes[boundary-1] == ' ' || runes[boundary-1] == '\n' {
					end = boundary
					break
				}
			}
		}
		part := strings.TrimSpace(string(runes[:end]))
		if part != "" {
			parts = append(parts, part)
		}
		runes = runes[end:]
		for len(runes) > 0 && (runes[0] == ' ' || runes[0] == '\n') {
			runes = runes[1:]
		}
	}
	return parts
}

func (service *Service) sendMessage(ctx context.Context, chatID int64, text string) (int64, bool, error) {
	if !service.allowedChat(chatID) {
		return 0, false, fmt.Errorf("Telegram destination is not allowed")
	}
	text = strings.TrimSpace(text)
	if chatID == 0 || text == "" || len([]rune(text)) > 4096 {
		return 0, false, fmt.Errorf("valid chat ID and message text of at most 4096 characters are required")
	}
	body, err := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	if err != nil {
		return 0, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, service.apiOrigin+"/bot"+service.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("create Telegram API request")
	}
	request.Header.Set("Content-Type", "application/json")
	var response struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := service.call(request, &response); err != nil {
		return 0, false, err
	}
	if !response.OK || response.Result.MessageID < 1 {
		return 0, false, fmt.Errorf("Telegram sendMessage failed: %s", response.Description)
	}
	historyEmbedded := true
	if err := service.store.RememberTelegramMessage(ctx, chatID, 0, response.Result.MessageID, "outgoing", text); err != nil {
		historyEmbedded = false
		log.Printf("Telegram message sent but history embedding failed: %v", err)
	}
	return response.Result.MessageID, historyEmbedded, nil
}

func (service *Service) call(request *http.Request, output any) error {
	response, err := service.http.Do(request)
	if err != nil {
		return fmt.Errorf("call Telegram API: request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return fmt.Errorf("read Telegram response: %w", err)
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("Telegram response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Telegram API returned HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode Telegram response: %w", err)
	}
	return nil
}
