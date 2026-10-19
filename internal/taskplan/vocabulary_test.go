package taskplan_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func Test014AdmittedPlanRetainsBytesAndDigestAcrossVocabularyUpgrade(t *testing.T) {
	p, err := taskplan.Resolve(request("change"))
	if err != nil {
		t.Fatal(err)
	}
	p.Version, p.Type, p.Executor, p.Execution.Mode = 2, "ship", "rider", "mount"
	originalBrief := strings.ReplaceAll(string(p.Inputs[0].Bytes), "type: change", "type: ship")
	p.Inputs[0] = taskplan.Select("brief", p.Inputs[0].Source, []byte(originalBrief))
	for i := range p.Grants {
		if p.Grants[i].Name == "autonomy.merge" {
			p.Grants[i].Name = "autonomy.land"
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	original := strings.Replace(string(raw), `"pool":`, `"remuda":`, 1)
	digest := sha256.Sum256([]byte(original))
	upgraded, err := taskplan.Decode(original)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Type != "change" || upgraded.Executor != "worker" || upgraded.Execution.Mode != "sandbox" {
		t.Fatalf("interpreted plan not upgraded: %#v", upgraded)
	}
	if upgraded.SHA256() != hex.EncodeToString(digest[:]) {
		t.Fatal("upgrade changed an admitted digest")
	}
	encoded, err := upgraded.Encode()
	if err != nil || encoded != original || string(upgraded.Inputs[0].Bytes) != originalBrief {
		t.Fatalf("upgrade rewrote immutable plan evidence: %v", err)
	}
	revised, err := upgraded.Reconfigure(upgraded.Launch)
	if err != nil || revised.Revision != 2 || revised.PreviousSHA256 != upgraded.SHA256() {
		t.Fatalf("reconfigure lost legacy provenance: %#v %v", revised, err)
	}
}
