package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// catalog api/signal/catalog.json 的映射。
type catalog struct {
	ProtocolVersions struct {
		Min     int `json:"min"`
		Max     int `json:"max"`
		Current int `json:"current"`
	} `json:"protocol_versions"`
	CloseCodes  []intCode `json:"close_codes"`
	ErrorCodes  []strCode `json:"error_codes"`
	KickReasons []strCode `json:"kick_reasons"`
}

// strCode 字符串常量条目（错误码 / kick reason）。
type strCode struct {
	GoName  string `json:"go_name"`
	Value   string `json:"value"`
	Comment string `json:"comment"`
	Since   int    `json:"since"`
}

// intCode 整数常量条目（close code）。
type intCode struct {
	GoName  string `json:"go_name"`
	Value   int    `json:"value"`
	Comment string `json:"comment"`
	Since   int    `json:"since"`
}

// propEntry 保留 schema properties 的源顺序（Go map 无序，结构体字段序须确定）。
type propEntry struct {
	Name string
	P    propSchema
}

// propList 按 schema 中的声明顺序解码 properties。
type propList []propEntry

func (l *propList) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("properties 不是对象")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := kt.(string)
		if !ok {
			return fmt.Errorf("properties 键不是字符串")
		}
		var p propSchema
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*l = append(*l, propEntry{Name: name, P: p})
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// propSchema 单个属性的 schema 子集（只取生成器需要的键）。
type propSchema struct {
	Ref         string          `json:"$ref"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	GoType      string          `json:"x-go-type"`
	GoOmitEmpty bool            `json:"x-go-omitempty"`
	GoPointer   bool            `json:"x-go-pointer"`
	Items       json.RawMessage `json:"items"`
}

// typeDef 组件或消息载荷的结构定义。
type typeDef struct {
	Name        string // Go 类型名
	WireType    string // 消息 wire 值（组件为空）
	GoConst     string // 消息类型常量名（组件为空）
	Direction   string // C2S / S2C / BOTH（组件为空）
	Since       int
	Description string
	NoPayload   bool   // x-go-payload: none → 不生成结构体
	Ref         string // 根级 $ref：载荷即共享组件
	Properties  propList
	Required    map[string]bool
}

// msgFile messages/*.schema.json 的映射（键名走 x-* 扩展）。
type msgFile struct {
	Title       string   `json:"title"`
	WireType    string   `json:"x-wire-type"`
	GoConst     string   `json:"x-go-const"`
	Direction   string   `json:"x-direction"`
	Since       int      `json:"x-since"`
	Description string   `json:"description"`
	GoPayload   string   `json:"x-go-payload"`
	Ref         string   `json:"$ref"`
	Properties  propList `json:"properties"`
	Required    []string `json:"required"`
}

func loadCatalog(path string) (*catalog, error) {
	var c catalog
	if err := loadJSON(path, &c); err != nil {
		return nil, err
	}
	if c.ProtocolVersions.Min < 1 || c.ProtocolVersions.Max < c.ProtocolVersions.Min {
		return nil, fmt.Errorf("%s: protocol_versions 非法", path)
	}
	return &c, nil
}

// loadComponents 解析 components.schema.json 的 $defs。
func loadComponents(path string) (map[string]*typeDef, error) {
	var raw struct {
		Defs map[string]struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Properties  propList `json:"properties"`
			Required    []string `json:"required"`
		} `json:"$defs"`
	}
	if err := loadJSON(path, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]*typeDef, len(raw.Defs))
	for name, d := range raw.Defs {
		td := &typeDef{Name: name, Description: d.Description, Properties: d.Properties}
		if d.Title != "" {
			td.Name = d.Title
		}
		td.Required = reqSet(d.Required)
		out[name] = td
	}
	return out, nil
}

// loadMessages 扫描 messages/*.schema.json，按 wire 值排序保证输出确定。
func loadMessages(dir string) ([]*typeDef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*typeDef
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var f msgFile
		path := filepath.Join(dir, e.Name())
		if err := loadJSON(path, &f); err != nil {
			return nil, err
		}
		if f.WireType == "" || f.GoConst == "" || f.Title == "" {
			return nil, fmt.Errorf("%s: 缺 x-wire-type / x-go-const / title", path)
		}
		out = append(out, &typeDef{
			Name:        f.Title,
			WireType:    f.WireType,
			GoConst:     f.GoConst,
			Direction:   f.Direction,
			Since:       f.Since,
			Description: f.Description,
			NoPayload:   f.GoPayload == "none",
			Ref:         f.Ref,
			Properties:  f.Properties,
			Required:    reqSet(f.Required),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WireType < out[j].WireType })
	return out, nil
}

func reqSet(req []string) map[string]bool {
	m := make(map[string]bool, len(req))
	for _, r := range req {
		m[r] = true
	}
	return m
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
