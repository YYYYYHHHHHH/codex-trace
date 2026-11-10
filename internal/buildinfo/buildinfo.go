// Package buildinfo 保存构建时通过 -ldflags -X 注入的值。
// 源码中凭证保持为空，真实值只在 make dist 时从 build.env 注入。
package buildinfo

var (
	Version           = "dev"
	LangfuseHost      = ""
	LangfusePublicKey = ""
	LangfuseSecretKey = ""
	Environment       = ""
)
