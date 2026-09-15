package main

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// flattenNamespaceTools 把 Responses 请求 tools 里的 namespace 工具展开成顶层 function 工具。
//
// 宿主的 Responses→Interactions 转换把 namespace 转成 {"function_declarations":[...]} 分组，
// 而 devin 执行器只读取每个工具的 name/description/parameters，分组会变成一个无名工具，
// 上游直接返回 invalid_argument。展开后工具名保持子工具原名：AI SDK 按工具名路由调用结果，
// namespace 只是附加元数据。与已有顶层工具重名的子工具会被跳过。
//
// 返回改写后的请求体、展开的 namespace 数量和展开出的子工具数量；没有 namespace 时原样返回。
func flattenNamespaceTools(body []byte) ([]byte, int, int) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, 0, 0
	}
	hasNamespace := false
	seen := make(map[string]bool)
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "namespace":
			hasNamespace = true
		case "function", "":
			if name := tool.Get("name").String(); name != "" {
				seen[name] = true
			}
		}
	}
	if !hasNamespace {
		return body, 0, 0
	}

	var items []string
	namespaces, flattened := 0, 0
	for _, tool := range tools.Array() {
		if tool.Get("type").String() != "namespace" {
			items = append(items, tool.Raw)
			continue
		}
		namespaces++
		children := tool.Get("tools")
		if !children.Exists() {
			children = tool.Get("children")
		}
		for _, child := range children.Array() {
			childType := child.Get("type").String()
			name := child.Get("name").String()
			if (childType != "function" && childType != "") || name == "" || seen[name] {
				continue
			}
			seen[name] = true
			raw := child.Raw
			if childType == "" {
				if out, err := sjson.Set(raw, "type", "function"); err == nil {
					raw = out
				}
			}
			items = append(items, raw)
			flattened++
		}
	}

	out, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(items, ",")+"]"))
	if err != nil {
		return body, 0, 0
	}
	return out, namespaces, flattened
}
