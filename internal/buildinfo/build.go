// Package buildinfo 记录发布输入摘要，供后台、客户端和安装器核对实际运行版本。
package buildinfo

var SourceDigest = "development"
var Revision = "unversioned"
var BuiltAt = ""

type Info struct {
	SourceDigest string `json:"source_digest"`
	Revision     string `json:"revision"`
	BuiltAt      string `json:"built_at"`
}

func Current() Info { return Info{SourceDigest: SourceDigest, Revision: Revision, BuiltAt: BuiltAt} }
