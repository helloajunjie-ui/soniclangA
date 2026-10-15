package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openAIAdapter speaks the OpenAI-compatible /chat/completions, /embeddings and
// /models protocols (covers OpenAI/DeepSeek/Moonshot/智谱/通义/Ollama/vLLM/custom).
// It performs in-vendor key rotation with cooldown on failure; cross-vendor
// failover is handled by orchestrator/failover (DESIGN §6.6).
type openAIAdapter struct {
	*keyring
}

func (a *openAIAdapter) ID() string { return a.entity.ID }

// ---- ChatStream ----

func (a *openAIAdapter) ChatStream(ctx context.Context, messages []Message, opts CallOptions) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		var lastErr error
		tries := len(a.entity.Keys) + 1
		for attempt := 0; attempt < tries; attempt++ {
			ref, err := a.selectKey()
			if err != nil {
				lastErr = err
				break
			}
			secret, err := a.secret(ref)
			if err != nil {
				lastErr = err
				a.markCooldown(ref)
				continue
			}
			ok, streamErr := a.streamOnce(ctx, ch, secret, messages, opts)
			if ok {
				return
			}
			lastErr = streamErr
			a.markCooldown(ref)
		}
		ch <- StreamChunk{Done: true, Err: lastErr}
	}()
	return ch, nil
}

func (a *openAIAdapter) streamOnce(ctx context.Context, ch chan StreamChunk, secret string, messages []Message, opts CallOptions) (bool, error) {
	reqBody := buildChatRequest(messages, opts)
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range a.entity.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(b))
	}
	a.parseSSE(resp.Body, ch)
	return true, nil
}

func (a *openAIAdapter) parseSSE(r io.Reader, ch chan StreamChunk) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	toolAcc := map[int]*ToolCall{}
	var finishReason, model string
	var usage Usage
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "[DONE]" {
			break
		}
		var evt struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		if evt.Model != "" {
			model = evt.Model
		}
		if len(evt.Choices) == 0 {
			if evt.Usage != nil {
				usage = Usage{PromptTokens: evt.Usage.PromptTokens, CompletionTokens: evt.Usage.CompletionTokens, TotalTokens: evt.Usage.TotalTokens}
			}
			continue
		}
		d := evt.Choices[0].Delta
		chunk := StreamChunk{}
		if d.Content != "" {
			chunk.Delta = d.Content
		}
		if d.ReasoningContent != "" {
			chunk.Reasoning = d.ReasoningContent
		} else if d.Reasoning != "" {
			chunk.Reasoning = d.Reasoning
		}
		for _, tc := range d.ToolCalls {
			acc, ok := toolAcc[tc.Index]
			if !ok {
				acc = &ToolCall{ID: tc.ID, Name: tc.Function.Name}
				toolAcc[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			acc.Arguments += tc.Function.Arguments
		}
		if chunk.Delta != "" || chunk.Reasoning != "" {
			ch <- chunk
		}
		if evt.Choices[0].FinishReason != "" {
			finishReason = evt.Choices[0].FinishReason
		}
		if evt.Usage != nil {
			usage = Usage{PromptTokens: evt.Usage.PromptTokens, CompletionTokens: evt.Usage.CompletionTokens, TotalTokens: evt.Usage.TotalTokens}
		}
	}
	var tcs []ToolCall
	for _, v := range toolAcc {
		tcs = append(tcs, *v)
	}
	ch <- StreamChunk{Done: true, ToolCalls: tcs, Usage: usage, Model: model, FinishReason: finishReason}
}

// ---- Chat (non-stream) ----

func (a *openAIAdapter) Chat(ctx context.Context, messages []Message, opts CallOptions) (*ChatResponse, error) {
	reqBody := buildChatRequest(messages, opts)
	reqBody.Stream = false
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.firstSecret())
	for k, v := range a.entity.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	resp2 := &ChatResponse{Model: out.Model}
	if len(out.Choices) > 0 {
		m := out.Choices[0].Message
		resp2.FinishReason = out.Choices[0].FinishReason
		resp2.Content = m.Content
		if m.ReasoningContent != "" {
			resp2.Content = m.ReasoningContent + "\n" + m.Content
		}
		for _, tc := range m.ToolCalls {
			resp2.ToolCalls = append(resp2.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	resp2.Usage = Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens, TotalTokens: out.Usage.TotalTokens}
	return resp2, nil
}

// ---- Embeddings ----

func (a *openAIAdapter) Embeddings(ctx context.Context, model string, texts []string) ([][]float32, error) {
	payload, _ := json.Marshal(map[string]interface{}{"input": texts, "model": model})
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.firstSecret())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	embs := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		embs = append(embs, d.Embedding)
	}
	return embs, nil
}

// ---- Models ----

func (a *openAIAdapter) Models(ctx context.Context) ([]ModelInfo, error) {
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.firstSecret())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	infos := make([]ModelInfo, 0, len(out.Data))
	for _, d := range out.Data {
		infos = append(infos, ModelInfo{ID: d.ID, Name: d.ID, Capabilities: ModelCaps{Chat: true}})
	}
	return infos, nil
}

// ---- request construction ----

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function oaFunc `json:"function"`
}

type oaFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFuncSpec `json:"function"`
}

type oaFuncSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type oaChatRequest struct {
	Model         string                 `json:"model"`
	Messages      []oaMessage            `json:"messages"`
	Stream        bool                   `json:"stream"`
	Temperature   *float64               `json:"temperature,omitempty"`
	MaxTokens     int                    `json:"max_tokens,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	Tools         []oaTool               `json:"tools,omitempty"`
	StreamOptions map[string]interface{} `json:"stream_options,omitempty"`
}

func buildChatRequest(messages []Message, opts CallOptions) oaChatRequest {
	out := oaChatRequest{
		Model:     opts.Model,
		Stream:    opts.Stream,
		MaxTokens: opts.MaxTokens,
	}
	if opts.Temperature != 0 {
		t := opts.Temperature
		out.Temperature = &t
	}
	if opts.TopP != 0 {
		p := opts.TopP
		out.TopP = &p
	}
	for _, m := range messages {
		om := oaMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: oaFunc{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		out.Messages = append(out.Messages, om)
	}
	for _, t := range opts.Tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out.Tools = append(out.Tools, oaTool{
			Type:     "function",
			Function: oaFuncSpec{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	if opts.Stream {
		out.StreamOptions = map[string]interface{}{"include_usage": true}
	}
	return out
}
