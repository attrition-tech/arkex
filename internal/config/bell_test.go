package config

import (
	"encoding/json"
	"testing"
)

func TestBellConfigMerge(t *testing.T) {
	c := &Config{UI: UI{Bell: new(true)}}
	c.merge(&Config{})
	if c.UI.Bell == nil || !*c.UI.Bell {
		t.Fatal("unset override disabled bell")
	}
	var override Config
	if err := json.Unmarshal([]byte(`{"ui":{"bell":false}}`), &override); err != nil {
		t.Fatal(err)
	}
	c.merge(&override)
	if c.UI.Bell == nil || *c.UI.Bell {
		t.Fatal("explicit false did not override")
	}
	data, err := json.Marshal(c.UI)
	if err != nil || string(data) != `{"bell":false}` {
		t.Fatalf("roundtrip: %s %v", data, err)
	}
}
