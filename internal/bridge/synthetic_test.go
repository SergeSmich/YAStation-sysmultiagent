package bridge

import (
	"strings"
	"testing"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"

	"quasarbridge/internal/wire"
)

func TestSyntheticTextInputBuildsDeviceShapedFrame(t *testing.T) {
	const text = "  Алиса, спроси нейросеть: расскажи анекдот  "
	frame, id, err := SyntheticTextInput(text)
	if err != nil {
		t.Fatalf("SyntheticTextInput: %v", err)
	}
	if id == "" {
		t.Fatalf("message id is empty")
	}
	decoded := wire.Decode(frame, wire.ClientToServer)
	if got := decoded.Kind(); got != "TextInput" {
		t.Fatalf("frame kind = %q, want TextInput", got)
	}
	event := decoded.Client.GetEvent()
	if event == nil {
		t.Fatalf("frame has no event")
	}
	if got := event.GetHeader().GetMessageId(); got != id {
		t.Fatalf("header message id = %q, want %q", got, id)
	}
	req := event.GetTextInput()
	if req == nil {
		t.Fatalf("frame has no text input")
	}
	if got := req.GetHeader().GetRequestId(); got != id {
		t.Fatalf("request id = %q, want %q", got, id)
	}
	if got := req.GetRequest().GetEvent().GetText(); got != strings.TrimSpace(text) {
		t.Fatalf("text = %q, want trimmed input", got)
	}
	if got := req.GetRequest().GetEvent().GetType(); got != common.EEventType_text_input {
		t.Fatalf("event type = %v, want text_input", got)
	}
}

func TestSyntheticTextInputUsesFreshIDs(t *testing.T) {
	first, firstID, err := SyntheticTextInput("первый вопрос")
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	second, secondID, err := SyntheticTextInput("второй вопрос")
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if firstID == secondID {
		t.Fatalf("duplicate message id %q: Native.Client would silently drop it", firstID)
	}
	if string(first) == string(second) {
		t.Fatalf("frames are identical")
	}
}

func TestSyntheticTextInputRejectsBadText(t *testing.T) {
	if _, _, err := SyntheticTextInput("   \n\t "); err == nil {
		t.Fatalf("empty text accepted")
	}
	if _, _, err := SyntheticTextInput(strings.Repeat("я", MaxSyntheticTextRunes+1)); err == nil {
		t.Fatalf("overlong text accepted")
	}
	if _, _, err := SyntheticTextInput(strings.Repeat("я", MaxSyntheticTextRunes)); err != nil {
		t.Fatalf("max length text rejected: %v", err)
	}
}
