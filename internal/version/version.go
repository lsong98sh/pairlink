// Package version 提供 PairLink 唯一的版本号常量。
//
// 版本号只有一个来源：main 的 -version 输出、三种模式管理页顶栏、日志里的
// version 字段都必须引用本常量，禁止各自写死字符串（阶段 3 曾出现 relay/agent
// 各自写死、client 管理页留空的不一致）。
//
// 取值带 "v" 前缀，与 git tag（v0.3.0）以及 `pairlink -version` 的既有输出
// `pairlink v0.3.0` 保持一致；需要裸版本号（如语义化版本比较）时用 Number。
package version

// Version 是完整版本号（含 "v" 前缀，与 git tag 一致）。
const Version = "v0.3.0"

// Number 是不带 "v" 前缀的版本号，供需要语义化版本格式的场景使用。
const Number = "0.3.0"
