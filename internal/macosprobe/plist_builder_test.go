package macosprobe

import (
	"bytes"
	"encoding/xml"
)

func probePlist(label string, args []string, logPath, session string) string {
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>`)
	// bytes.Buffer 的 Write 不失败；EscapeText 只做 XML 编码。
	_ = xml.EscapeText(&out, []byte(label))
	out.WriteString(`</string><key>LimitLoadToSessionType</key><string>`)
	_ = xml.EscapeText(&out, []byte(session))
	out.WriteString(`</string><key>ProgramArguments</key><array>`)
	for _, arg := range args {
		out.WriteString("<string>")
		_ = xml.EscapeText(&out, []byte(arg))
		out.WriteString("</string>")
	}
	out.WriteString(`</array><key>RunAtLoad</key><true/><key>KeepAlive</key><false/><key>AbandonProcessGroup</key><false/><key>ExitTimeOut</key><integer>2</integer>`)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		out.WriteString("<key>" + key + "</key><string>")
		_ = xml.EscapeText(&out, []byte(logPath))
		out.WriteString("</string>")
	}
	out.WriteString("</dict></plist>")
	return out.String()
}
