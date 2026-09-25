package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// deliveryEnvelope is the only presentation boundary for agent-directed text.
// The body is a JSON string on one physical line: even a newline followed by
// a forged header remains data, and JSON decoding recovers the exact text.
// Origin and classification come from the caller's trusted delivery path,
// never from the body. Text-only harnesses cannot authenticate a prompt typed
// by the User; consumers must not classify arbitrary text by prefix alone.
func deliveryEnvelope(from, to, task, purpose, ids, body string) string {
	encoded, _ := json.Marshal(body)
	return fmt.Sprintf("[posse | %s -> %s %s | %s #%s]\nbody: %s", from, to, task, purpose, ids, encoded)
}

func workerInstruction(task store.Task, message store.Message) string {
	return deliveryEnvelope("Lead", "Rider", taskIDString(task.Seq), "instruction", fmt.Sprint(message.ID), message.Body)
}

func noticeEnvelope(notices []store.Notice, tasks []string, body string) string {
	ids := make([]string, 0, len(notices))
	for _, notice := range notices {
		ids = append(ids, fmt.Sprint(notice.ID))
	}
	if len(tasks) == 0 {
		tasks = []string{"project"}
	}
	return deliveryEnvelope("Posse", "Lead", strings.Join(tasks, ","), "notice", strings.Join(ids, ","), body)
}
