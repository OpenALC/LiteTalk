package api

import (
	"encoding/json"
)

// JSON Merge Patch 三态字段载体（RFC 7396，#31）。
// 生成器对可选字段一律包 *T，json 对 null 直接置 nil 指针、进不了自定义
// UnmarshalJSON——所以整个请求体用 x-go-type 指到这里的结构，键存在性由
// decodeMergePatch 探测：缺省 = 不改（Set=false），null = 清空（Null=true），
// 值 = 设置。handler 按字段语义解释「清空」（remark/group 清串、category_id
// 移出分类、必填字段的空值被校验拒 400）。

// mergeField 三态字段的统一入口（OptString/OptInt/OptID 均实现）。
type mergeField interface{ UnmarshalJSON([]byte) error }

// decodeMergePatch 把对象体按键派给三态字段；键缺席保持 Set=false。
func decodeMergePatch(data []byte, fields map[string]mergeField) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for name, f := range fields {
		if v, ok := raw[name]; ok {
			if err := f.UnmarshalJSON(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// patchBody 双内容类型（application/json 与 application/merge-patch+json）归一：
// 同一 schema 两种 Content-Type 生成两个字段，取先到的一个。
func patchBody[T any](jsonBody, mergeBody *T) *T {
	if mergeBody != nil {
		return mergeBody
	}
	return jsonBody
}

// PatchMeBody PATCH /users/me 合并补丁体（仅 nickname）。
type PatchMeBody struct {
	Nickname OptString
}

// UnmarshalJSON 三态解码（键存在性见 decodeMergePatch）。
func (b *PatchMeBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{"nickname": &b.Nickname})
}

// PatchFriendBody PATCH /friends/{id} 合并补丁体（remark/group）。
type PatchFriendBody struct {
	Remark OptString
	Group  OptString
}

// UnmarshalJSON 三态解码。
func (b *PatchFriendBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{
		"remark": &b.Remark, "group": &b.Group,
	})
}

// PatchServerBody PATCH /servers/{id} 合并补丁体（name/icon_name/icon_color）。
type PatchServerBody struct {
	Name      OptString
	IconName  OptString
	IconColor OptString
}

// UnmarshalJSON 三态解码。
func (b *PatchServerBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{
		"name": &b.Name, "icon_name": &b.IconName, "icon_color": &b.IconColor,
	})
}

// PatchChannelBody PATCH /channels/{id} 合并补丁体（name/type/category_id/position）。
type PatchChannelBody struct {
	Name       OptString
	Type       OptString
	CategoryID OptID
	Position   OptInt
}

// UnmarshalJSON 三态解码。
func (b *PatchChannelBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{
		"name": &b.Name, "type": &b.Type,
		"category_id": &b.CategoryID, "position": &b.Position,
	})
}

// PatchCategoryBody PATCH /categories/{id} 合并补丁体（name/position）。
type PatchCategoryBody struct {
	Name     OptString
	Position OptInt
}

// UnmarshalJSON 三态解码。
func (b *PatchCategoryBody) UnmarshalJSON(data []byte) error {
	return decodeMergePatch(data, map[string]mergeField{
		"name": &b.Name, "position": &b.Position,
	})
}
