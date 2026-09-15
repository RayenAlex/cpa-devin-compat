package main

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// pinnedSessionIDPrefix 标记由本插件推导的会话 ID，便于在日志里和客户端自带的会话区分。
const pinnedSessionIDPrefix = "dvc_"

// pinnedSessionID 在请求没有任何显式会话标识时，返回按对话稳定的会话 ID；否则返回空串。
//
// 宿主只有在拿不到显式会话标识时才走 LCP 前缀匹配，而 LCP 一旦判定 fork 就换新会话 ID，
// Devin 的 cascade_id 随之改变，prompt cache 全部失效。这里复用宿主的 DeriveID
// （系统提示词 + 首条用户消息的哈希），同一段对话后续轮次、以及历史被改写的分叉，
// 都会得到同一个 ID。
func pinnedSessionID(headers http.Header, body []byte, metadata map[string]any, sourceFormat string) string {
	if len(body) == 0 {
		return ""
	}
	if info, ok := session.ExtractSessionInfo(headers, body, metadata); ok && info.ClientType != "lcp" && strings.TrimSpace(info.SessionID) != "" {
		return ""
	}
	id := session.DeriveID(sdktranslator.FromString(sourceFormat), body, "")
	if id == "" {
		return ""
	}
	return pinnedSessionIDPrefix + id
}
