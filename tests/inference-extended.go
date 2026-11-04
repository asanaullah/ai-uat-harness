// Assisted by Claude Opus
package test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The benchmark tests measure how fast a serving stack is; this one checks
// that it behaves. Latency is recorded, not asserted: the right budget
// depends on the workload, so the numbers go to the run artifacts.

var (
	serverURL string
	modelName string
	client    *http.Client
)

func TestInferenceExtended(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Inference Validation Suite")
}

var _ = BeforeSuite(func() {
	serverURL = os.Getenv("SERVER_URL")
	modelName = os.Getenv("MODEL_NAME")
	Expect(serverURL).NotTo(BeEmpty(), "SERVER_URL must be set")
	Expect(modelName).NotTo(BeEmpty(), "MODEL_NAME must be set")
	client = &http.Client{Timeout: 2 * time.Minute}
})

func post(path string, payload map[string]interface{}) (*http.Response, []byte) {
	raw, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	resp, err := client.Post(serverURL+path, "application/json", bytes.NewReader(raw))
	Expect(err).NotTo(HaveOccurred(), "reaching %s%s", serverURL, path)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp, body
}

func chatBody(content string, extra map[string]interface{}) map[string]interface{} {
	payload := map[string]interface{}{
		"model": modelName,
		"messages": []map[string]interface{}{
			{"role": "user", "content": content},
		},
		"temperature": 0,
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

func chatContent(content string, extra map[string]interface{}) (string, string) {
	resp, body := post("/v1/chat/completions", chatBody(content, extra))
	Expect(resp.StatusCode).To(Equal(200), "chat completion: %s", string(body))
	var parsed struct {
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	Expect(json.Unmarshal(body, &parsed)).To(Succeed(), "chat response is not JSON: %s", string(body))
	Expect(parsed.Choices).NotTo(BeEmpty(), "chat response has no choices: %s", string(body))
	return parsed.Choices[0].Message.Content, parsed.Choices[0].FinishReason
}

var _ = Describe("Inference Validation", Label("pass-fail"), func() {
	It("should list the model", func() {
		resp, err := client.Get(serverURL + "/v1/models")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(200), "listing models: %s", string(body))
		Expect(string(body)).To(ContainSubstring(modelName), "the served model is missing: %s", string(body))
	})

	It("should answer a factual question", func() {
		content, _ := chatContent("What is the capital of France? Answer with the city name only.", map[string]interface{}{"max_tokens": 20})
		Expect(content).To(ContainSubstring("Paris"),
			"the answer should name Paris; the model said %q", content)
	})

	It("should return the completions schema", func() {
		resp, body := post("/v1/completions", map[string]interface{}{
			"model": modelName, "prompt": "The capital of France is", "max_tokens": 10, "temperature": 0,
		})
		Expect(resp.StatusCode).To(Equal(200), "completion: %s", string(body))
		var parsed struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Text         string `json:"text"`
				Index        int    `json:"index"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		Expect(json.Unmarshal(body, &parsed)).To(Succeed(), "completion response is not JSON: %s", string(body))
		Expect(parsed.ID).NotTo(BeEmpty(), "no id: %s", string(body))
		Expect(parsed.Object).To(Equal("text_completion"), "wrong object: %s", string(body))
		Expect(parsed.Choices).To(HaveLen(1), "expected one choice: %s", string(body))
		Expect(parsed.Choices[0].Text).NotTo(BeEmpty(), "empty completion text: %s", string(body))
		Expect(parsed.Usage.PromptTokens).To(BeNumerically(">", 0), "no prompt tokens: %s", string(body))
		Expect(parsed.Usage.CompletionTokens).To(BeNumerically(">", 0), "no completion tokens: %s", string(body))
	})

	It("should return the chat completions schema", func() {
		resp, body := post("/v1/chat/completions", chatBody("Say hello.", map[string]interface{}{"max_tokens": 10}))
		Expect(resp.StatusCode).To(Equal(200), "chat completion: %s", string(body))
		var parsed struct {
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
				Index        int    `json:"index"`
			} `json:"choices"`
		}
		Expect(json.Unmarshal(body, &parsed)).To(Succeed(), "chat response is not JSON: %s", string(body))
		Expect(parsed.Choices).To(HaveLen(1), "expected one choice: %s", string(body))
		Expect(parsed.Choices[0].Message.Role).To(Equal("assistant"), "wrong role: %s", string(body))
		Expect(parsed.Choices[0].Message.Content).NotTo(BeEmpty(), "empty content: %s", string(body))
		Expect(parsed.Choices[0].FinishReason).NotTo(BeEmpty(), "no finish reason: %s", string(body))
	})

	It("should be deterministic at temperature zero", func() {
		first, _ := chatContent("Name three primary colors.", map[string]interface{}{"max_tokens": 30})
		second, _ := chatContent("Name three primary colors.", map[string]interface{}{"max_tokens": 30})
		Expect(first).To(Equal(second),
			"temperature zero gave different answers: %q vs %q", first, second)
	})

	It("should stream chunks terminated by DONE", func() {
		payload := chatBody("Count from one to five.", map[string]interface{}{"max_tokens": 40, "stream": true})
		raw, err := json.Marshal(payload)
		Expect(err).NotTo(HaveOccurred())
		start := time.Now()
		resp, err := client.Post(serverURL+"/v1/chat/completions", "application/json", bytes.NewReader(raw))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(200))

		assembled := ""
		chunks := 0
		sawDone := false
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				sawDone = true
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			Expect(json.Unmarshal([]byte(data), &chunk)).To(Succeed(), "bad stream chunk: %s", data)
			chunks++
			if len(chunk.Choices) > 0 {
				assembled += chunk.Choices[0].Delta.Content
			}
		}
		Expect(scanner.Err()).NotTo(HaveOccurred(), "reading the stream")
		Expect(sawDone).To(BeTrue(), "the stream never ended with [DONE]")
		Expect(chunks).To(BeNumerically(">", 0), "no chunks arrived")
		Expect(assembled).NotTo(BeEmpty(), "the stream carried no content")
		AddReportEntry("stream_chunks", chunks)
		AddReportEntry("stream_seconds", time.Since(start).Seconds())
	})

	It("should reject an unknown model", func() {
		resp, body := post("/v1/chat/completions", map[string]interface{}{
			"model": "no-such-model", "messages": []map[string]interface{}{{"role": "user", "content": "hi"}},
		})
		Expect(resp.StatusCode).To(Equal(404), "unknown model: %s", string(body))
	})

	It("should reject a request without messages", func() {
		resp, body := post("/v1/chat/completions", map[string]interface{}{"model": modelName})
		Expect(resp.StatusCode).To(Equal(400), "missing messages: %s", string(body))
	})

	It("should honor a stop sequence", func() {
		resp, body := post("/v1/completions", map[string]interface{}{
			"model": modelName, "prompt": "The capital of France is",
			"max_tokens": 10, "temperature": 0, "stop": []string{"Paris"},
		})
		Expect(resp.StatusCode).To(Equal(200), "stop completion: %s", string(body))
		var parsed struct {
			Choices []struct {
				Text         string `json:"text"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		Expect(json.Unmarshal(body, &parsed)).To(Succeed(), "stop response is not JSON: %s", string(body))
		Expect(parsed.Choices).To(HaveLen(1), "expected one choice: %s", string(body))
		Expect(parsed.Choices[0].Text).NotTo(ContainSubstring("Paris"),
			"the stop sequence leaked into the output: %q", parsed.Choices[0].Text)
		Expect(parsed.Choices[0].FinishReason).To(Equal("stop"), "finish reason: %s", string(body))
	})

	It("should record serving latency", func() {
		// Quantitative, not pass-fail: the budget depends on the workload.
		payload := chatBody("Write one sentence about the sea.", map[string]interface{}{"max_tokens": 60, "stream": true})
		raw, err := json.Marshal(payload)
		Expect(err).NotTo(HaveOccurred())
		start := time.Now()
		var ttft time.Duration
		ttftSeen := false
		resp, err := client.Post(serverURL+"/v1/chat/completions", "application/json", bytes.NewReader(raw))
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(200))
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" && !ttftSeen {
				ttft = time.Since(start)
				ttftSeen = true
			}
		}
		Expect(scanner.Err()).NotTo(HaveOccurred(), "reading the stream")
		Expect(ttftSeen).To(BeTrue(), "no content arrived to time")
		AddReportEntry("ttft_seconds", ttft.Seconds())
		AddReportEntry("total_seconds", time.Since(start).Seconds())
	})
})
