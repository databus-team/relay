package portfwd

import (
	"encoding/json"
	"reflect"
	"testing"
)

// wireEnvelope 复刻任务简报/执行请求上的承载形态:{ "ports": [...] }。
type wireEnvelope struct {
	Ports []Spec `json:"ports,omitempty"`
}

// TestParse_WireRoundTripIsFieldLossless 覆盖 R21/KTD9 的核心:解析结果经 wire 往返后逐字段相等,
// 绑定面必须原样带走(展示字符串丢掉的正是它)。
func TestParse_WireRoundTripIsFieldLossless(t *testing.T) {
	cases := []struct {
		raw      string
		wantBind string
		wantSpec Spec
	}{
		{
			raw:      "8080",
			wantBind: "127.0.0.1",
			wantSpec: Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080},
		},
		{
			raw:      "8080:9000",
			wantBind: "127.0.0.1",
			wantSpec: Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000},
		},
		{
			raw:      "0.0.0.0:8080",
			wantBind: "0.0.0.0",
			wantSpec: Spec{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080},
		},
		{
			raw:      "0.0.0.0:8080:9000",
			wantBind: "0.0.0.0",
			wantSpec: Spec{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000},
		},
		{
			raw:      "8080:db.local:5432",
			wantBind: "127.0.0.1",
			wantSpec: Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "db.local", RemotePort: 5432},
		},
	}

	for _, c := range cases {
		spec, err := Parse(c.raw, DefaultBind)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.raw, err)
		}
		if *spec != c.wantSpec {
			t.Fatalf("Parse(%q) = %+v, want %+v", c.raw, *spec, c.wantSpec)
		}

		raw, err := json.Marshal(wireEnvelope{Ports: []Spec{*spec}})
		if err != nil {
			t.Fatalf("marshal %q: %v", c.raw, err)
		}
		var got wireEnvelope
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal %q (%s): %v", c.raw, raw, err)
		}
		if len(got.Ports) != 1 || got.Ports[0] != c.wantSpec {
			t.Errorf("%q round-trip = %+v, want %+v", c.raw, got.Ports, c.wantSpec)
		}
		if got.Ports[0].BindHost != c.wantBind {
			t.Errorf("%q round-trip bind = %q, want %q", c.raw, got.Ports[0].BindHost, c.wantBind)
		}
	}
}

// TestUnmarshalJSON_WireIsAlwaysObject 钉住上行形态:结构化对象,而非 String() 的产物。
func TestUnmarshalJSON_WireIsAlwaysObject(t *testing.T) {
	spec, err := Parse("0.0.0.0:8080", DefaultBind)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"bind":"0.0.0.0","local_port":8080,"remote_host":"127.0.0.1","remote_port":8080}`
	if string(raw) != want {
		t.Errorf("wire form = %s, want %s", raw, want)
	}
}

// TestUnmarshalJSON_LegacyStringForm 覆盖老执行方上行与老 meta.json 的展示字符串形态:
// 必须解得开,且按历史默认绑定 127.0.0.1 还原(它们丢掉的只有绑定面)。
func TestUnmarshalJSON_LegacyStringForm(t *testing.T) {
	cases := []struct {
		legacy string
		want   Spec
	}{
		{
			legacy: "8080",
			want:   Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080},
		},
		{
			legacy: "8080:9000",
			want:   Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000},
		},
		{
			legacy: "8080:db.local:5432",
			want:   Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "db.local", RemotePort: 5432},
		},
	}

	for _, c := range cases {
		payload := `{"ports":["` + c.legacy + `"]}`
		var got wireEnvelope
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("unmarshal legacy %s: %v", payload, err)
		}
		if len(got.Ports) != 1 || got.Ports[0] != c.want {
			t.Errorf("legacy %s = %+v, want %+v", payload, got.Ports, c.want)
		}
	}
}

// TestUnmarshalJSON_MixedForms 覆盖同一数组里新旧形态混排(执行方滚动升级期间的现实形态)。
func TestUnmarshalJSON_MixedForms(t *testing.T) {
	payload := `{"ports":["8080",{"bind":"0.0.0.0","local_port":9090,"remote_host":"127.0.0.1","remote_port":9090}]}`
	var got wireEnvelope
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal mixed: %v", err)
	}
	want := []Spec{
		{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080},
		{BindHost: "0.0.0.0", LocalPort: 9090, RemoteHost: "127.0.0.1", RemotePort: 9090},
	}
	if !reflect.DeepEqual(got.Ports, want) {
		t.Errorf("mixed forms = %+v, want %+v", got.Ports, want)
	}
}

// TestUnmarshalJSON_InvalidLegacyStringIsAnError 覆盖失效的旧字符串:报错而不是静默落成零值规格。
func TestUnmarshalJSON_InvalidLegacyStringIsAnError(t *testing.T) {
	var got wireEnvelope
	if err := json.Unmarshal([]byte(`{"ports":["not-a-port"]}`), &got); err == nil {
		t.Fatalf("expected error for legacy garbage, got %+v", got.Ports)
	}
}

// TestSpec_StringIsDisplayOnly 钉住展示语义:绑定主机按设计不出现在 String() 里。
func TestSpec_StringIsDisplayOnly(t *testing.T) {
	cases := []struct {
		spec Spec
		want string
	}{
		{Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080}, "8080"},
		{Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 9000}, "8080:9000"},
		{Spec{BindHost: "0.0.0.0", LocalPort: 8080, RemoteHost: "127.0.0.1", RemotePort: 8080}, "8080"},
		{Spec{BindHost: "127.0.0.1", LocalPort: 8080, RemoteHost: "db.local", RemotePort: 5432}, "8080:db.local:5432"},
	}
	for _, c := range cases {
		if got := c.spec.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.spec, got, c.want)
		}
	}
}

// TestParse_RejectsUnchangedGrammar 确认搬进本包的语法没有放宽:既有拒绝用例照旧拒绝。
func TestParse_RejectsUnchangedGrammar(t *testing.T) {
	for _, raw := range []string{"invalid", "0:8080", "8080:70000", ":8080", "a:b:c:d", ""} {
		if spec, err := Parse(raw, DefaultBind); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", raw, *spec)
		}
	}
}

// TestParse_DefaultBindFallback 确认空 defaultBind 回落到历史默认值。
func TestParse_DefaultBindFallback(t *testing.T) {
	spec, err := Parse("8080", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.BindHost != DefaultBind {
		t.Errorf("BindHost = %q, want %q", spec.BindHost, DefaultBind)
	}
}
