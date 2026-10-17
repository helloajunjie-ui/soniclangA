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
	"time"
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
		var lastErr, firstErr error
		tries := len(a.entity.Keys) + 1
		for attempt := 0; attempt < tries; attempt++ {
			ref, err := a.selectKey()
			if err != nil {
				// Report the first real failure, not the retry bookkeeping. With
				// a single key the failed attempt benches it, so this branch says
				// "all keys in cooldown" — which hides the 401/500 that actually
				// caused it behind a message the user cannot act on.
				if firstErr != nil {
					lastErr = firstErr
				} else {
					lastErr = err
				}
				break
			}
			secret, err := a.secret(ref)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				lastErr = err
				a.markCooldown(ref)
				continue
			}
			ok, streamErr := a.streamOnce(ctx, ch, secret, messages, opts)
			if ok {
				return
			}
			if firstErr == nil {
				firstErr = streamErr
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
	client := &http.Client{
		Timeout: 300 * time.Second,
	}
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

// ---- Image generation (OpenAI-compatible /images/generations) ----

func (a *openAIAdapter) GenerateImage(ctx context.Context, model, prompt string, opts ImageOpts) (*ImageResult, error) {
	// OpenAI-compatible image generation. Aggregation layers (ocoolai-style) are
	// picky: they key on User-Agent and expect vendor-specific fields
	// (output_format/background) for the gpt-image family, and only return the
	// body after a long (25-35s) generation. AQBOT sends a branded UA and these
	// fields; we mirror that so the response actually comes back instead of the
	// upstream closing the connection after billing.
	l := strings.ToLower(model)
	base := strings.ToLower(a.entity.BaseURL)
	isARK := strings.Contains(base, "ark.cn-beijing.volces.com") || strings.Contains(base, "volces.com")
	body := map[string]interface{}{"model": model, "prompt": prompt}
	n := opts.N
	if n < 1 {
		n = 1
	}
	body["n"] = n
	if opts.Size != "" {
		body["size"] = opts.Size
	}
	if opts.Quality != "" {
		body["quality"] = opts.Quality
	}
	if isARK {
		// Volcengine Ark (Doubao) images: no output_format; it wants
		// response_format=url + watermark, and size like "2K"/"4K"/"2048x2048".
		body["response_format"] = "url"
		body["watermark"] = true
	} else if !strings.Contains(l, "dall") {
		// gpt-image family takes output_format/background; DALL·E must not receive them.
		of := opts.OutputFormat
		if of == "" {
			of = "png"
		}
		body["output_format"] = of
		if opts.Background != "" {
			body["background"] = opts.Background
		}
	}
	// Image-to-image: pass the reference picture(s) through. Single URL goes as
	// a string; the multi-reference form (e.g. seedream 4.0 img2img) as an array.
	if len(opts.Images) > 0 {
		body["image"] = opts.Images
	} else if opts.Image != "" {
		body["image"] = opts.Image
	}
	payload, _ := json.Marshal(body)
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/images/generations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.firstSecret())
	req.Header.Set("User-Agent", "AQBot-compat/1.0")
	// Image generation routinely takes 25-35s upstream; do not cut it short.
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("image upstream %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
			Revised string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("image upstream: empty data")
	}
	d := out.Data[0]
	res := &ImageResult{URL: d.URL, B64: d.B64JSON, Model: model, Prompt: prompt}
	if len(out.Data) > 1 {
		urls := make([]string, 0, len(out.Data))
		for _, it := range out.Data {
			if it.URL != "" {
				urls = append(urls, it.URL)
			}
		}
		if len(urls) > 0 {
			res.Images = urls
		}
	}
	return res, nil
}

// ---- Video generation (OpenAI-compatible /videos/generations, async task) ----

func (a *openAIAdapter) GenerateVideo(ctx context.Context, model, prompt string) (*VideoResult, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model":  model,
		"prompt": prompt,
	})
	url := strings.TrimRight(a.entity.BaseURL, "/") + "/videos/generations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.firstSecret())
	// A generation call that never returns would otherwise hang the request
	// forever (the bare http.Client{} had no timeout). Video creation is usually
	// async — the upstream returns a task id quickly — but a synchronous variant
	// can take as long as image generation, so bound it the same way.
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("video upstream %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
		TaskID string `json:"task_id"`
		Task   string `json:"task"`
		ID     string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := &VideoResult{Model: model, Prompt: prompt}
	if len(out.Data) > 0 && out.Data[0].URL != "" {
		res.URL = out.Data[0].URL
	}
	if res.URL == "" {
		res.Task = out.TaskID
		if res.Task == "" {
			res.Task = out.Task
		}
		if res.Task == "" {
			res.Task = out.ID
		}
	}
	// ARK (Doubao seedance) video creation is async: the submit call returns a
	// task id and the finished clip only appears on the task endpoint. Poll it
	// (a read-only GET, no extra billing) for up to ~60s; if it never resolves
	// we hand the task id back and the caller can decide, rather than a dead end
	// where a generated video is never fetched.
	if res.Task != "" && isARKBase(a.entity.BaseURL) {
		if u := a.pollARKVideoTask(ctx, res.Task); u != "" {
			res.URL = u
		}
	}
	if res.URL == "" && res.Task == "" {
		return nil, fmt.Errorf("video upstream: no url or task id in response")
	}
	return res, nil
}

// isARKBase reports whether the base URL belongs to Volcengine Ark, which uses
// a different image/audio/video generation surface than plain OpenAI.
func isARKBase(base string) bool {
	b := strings.ToLower(base)
	return strings.Contains(b, "ark.cn-beijing.volces.com") || strings.Contains(b, "volces.com")
}

// pollARKVideoTask polls the Volcengine Ark task endpoint for a finished video
// URL. It is read-only and stops early on failure/success-without-url. The
// endpoint shape is documented at
// https://www.volcengine.com/docs/82379/1521309 ("查询视频生成任务").
func (a *openAIAdapter) pollARKVideoTask(ctx context.Context, taskID string) string {
	base := strings.TrimRight(a.entity.BaseURL, "/")
	endpoint := base + "/contents/generations/tasks/" + taskID
	for i := 0; i < 12; i++ { // ~60s at a 5s interval
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(5 * time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Authorization", "Bearer "+a.firstSecret())
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		var out struct {
			Status   string `json:"status"`
			URL      string `json:"url"`
			VideoURL string `json:"video_url"`
			Data     []struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &out) != nil {
			continue
		}
		if u := out.VideoURL; u != "" {
			return u
		}
		if u := out.URL; u != "" {
			return u
		}
		if len(out.Data) > 0 && out.Data[0].URL != "" {
			return out.Data[0].URL
		}
		st := strings.ToLower(out.Status)
		if st == "failed" || st == "error" {
			return ""
		}
		// "success" without a URL field we recognise, or queued/running: keep polling.
	}
	return ""
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
		infos = append(infos, ModelInfo{ID: d.ID, Name: d.ID, Capabilities: detectOpenAICaps(d.ID)})
	}
	return infos, nil
}

// detectOpenAICaps infers a model's capabilities from its id when the /models
// endpoint returns nothing but ids (the usual case for OpenAI-compatible
// vendors). The user should not have to tell us "this one is multimodal, that
// one reasons": the runtime derives it, so a vision-capable image in chat just
// works and a reasoning model surfaces its thinking without any config.
func detectOpenAICaps(id string) ModelCaps {
	caps := ModelCaps{Chat: true}
	l := strings.ToLower(id)
	// Multimodal / vision-capable: 4o and 4-vision (OpenAI), vision / vl (Qwen,
	// InternVL, CogVLM, MiniCPM-V, glm-4v), omm / 1.5 vision, gemini-pro-vision.
	if strings.Contains(l, "4o") || strings.Contains(l, "vision") ||
		strings.Contains(l, "-vl") || strings.Contains(l, "4v") ||
		strings.Contains(l, "glm-4v") || strings.Contains(l, "omm") ||
		strings.Contains(l, "internvl") || strings.Contains(l, "minicpm-v") {
		caps.Vision = true
	}
	// Reasoning-capable: o-series (o1/o3/o4), deepseek-reasoner, r1 / deepseek-v4
	// (reasoning-flash), qwen-thinking, kimi-k2-thinking, thinking / reasoner / sonnet-o1.
	if strings.Contains(l, "reasoner") || strings.Contains(l, "reasoning") ||
		strings.Contains(l, "thinking") || strings.Contains(l, "o1-") ||
		strings.Contains(l, "o3") || strings.Contains(l, "o4") ||
		strings.Contains(l, "-r1") || strings.Contains(l, "r1-") ||
		strings.Contains(l, "deepseek-v4") || strings.Contains(l, "k2-thinking") {
		caps.Reasoning = true
	}
	return caps
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
	Model           string                 `json:"model"`
	Messages        []oaMessage            `json:"messages"`
	Stream          bool                   `json:"stream"`
	Temperature     *float64               `json:"temperature,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	TopP            *float64               `json:"top_p,omitempty"`
	Tools           []oaTool               `json:"tools,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
	Thinking        *oaThinking            `json:"thinking,omitempty"`
	StreamOptions   map[string]interface{} `json:"stream_options,omitempty"`
}

// oaThinking is DeepSeek's reasoning control: type enabled|disabled plus an
// optional token budget. DeepSeek has no low/high reasoning_effort, so the
// strength selector is translated into budget_tokens instead.
type oaThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
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
	// Reasoning strength is a single selector (auto|off|light|deep) that each
	// model family translates into its own native control. The selector is only
	// applied when the model is actually reasoning-capable: sending a DeepSeek
	// `thinking` field to a non-reasoning model (e.g. deepseek-flash) makes the
	// upstream hang instead of answering — the costliest possible failure for a
	// strength knob. detectOpenAICaps flags reasoning models; it is the same
	// inference used to surface thinking in the first place.
	if opts.ReasoningEffort != "" && opts.ReasoningEffort != "auto" && detectOpenAICaps(opts.Model).Reasoning {
		l := strings.ToLower(opts.Model)
		switch {
		case strings.Contains(l, "deepseek"):
			t := &oaThinking{Type: "enabled"}
			switch opts.ReasoningEffort {
			case "off":
				t.Type = "disabled"
			case "light":
				b := 2048
				t.BudgetTokens = &b
			case "deep":
				b := 8192
				t.BudgetTokens = &b
			}
			out.Thinking = t
		case strings.Contains(l, "o1-") || strings.Contains(l, "o3") || strings.Contains(l, "o4"):
			switch opts.ReasoningEffort {
			case "light":
				out.ReasoningEffort = "low"
			case "deep":
				out.ReasoningEffort = "high"
			}
		}
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
