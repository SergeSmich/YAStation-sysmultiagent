package main

// llmPreset is a ready-made provider the panel offers in the «Сервис» list.
// It fills the address and the wire format; the model is chosen from the list
// the provider returns. Qwen has two regions because a DashScope key only works
// in the region it was issued in, and the key shape does not tell them apart.
type llmPreset struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	API     string `json:"api"`
}

var llmPresets = []llmPreset{
	{ID: "groq", Label: "Groq", Name: "groq", BaseURL: "https://api.groq.com/openai/v1", API: "chat_completions"},
	{ID: "qwen-intl", Label: "Qwen (Alibaba, международный)", Name: "qwen", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", API: "chat_completions"},
	{ID: "qwen-cn", Label: "Qwen (Alibaba, Китай)", Name: "qwen-cn", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", API: "chat_completions"},
	{ID: "gigachat", Label: "GigaChat (Сбер)", Name: "gigachat", BaseURL: "https://gigachat.devices.sberbank.ru/api/v1", API: "gigachat"},
	{ID: "deepseek", Label: "DeepSeek", Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", API: "chat_completions"},
	{ID: "openrouter", Label: "OpenRouter", Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", API: "chat_completions"},
	{ID: "google", Label: "Google Gemini", Name: "google", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", API: "chat_completions"},
	{ID: "openai", Label: "OpenAI", Name: "openai", BaseURL: "https://api.openai.com/v1", API: "chat_completions"},
	{ID: "xai", Label: "xAI", Name: "xai", BaseURL: "https://api.x.ai/v1", API: "chat_completions"},
	{ID: "mistral", Label: "Mistral", Name: "mistral", BaseURL: "https://api.mistral.ai/v1", API: "chat_completions"},
}

func llmPresetByID(id string) (llmPreset, bool) {
	for _, p := range llmPresets {
		if p.ID == id {
			return p, true
		}
	}
	return llmPreset{}, false
}
