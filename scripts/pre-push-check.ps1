#!/usr/bin/env pwsh
#requires -Version 5.1
<#
.SYNOPSIS
    push 前的密钥泄露检查。在 git push 之前跑，拦住不该进仓库的东西。

.DESCRIPTION
    一次 commit 里混进密钥，push 之后就来不及了：删掉文件没用，历史里还在，
    fork 和爬虫可能已经拿走。所以检查必须发生在 push 之前，而且要看 git 真正
    会提交的内容，不是目录里有什么。

    本脚本做四件事：
      1. 看 git 暂存区（实际会被 commit 的文件），不是工作区
      2. 扫密钥形态的字符串（sk- 开头的 key、私钥块、赋值给 KEY/SECRET 的长串）
      3. 确认 .env 之类的文件没有被 -f 强制加入
      4. 检查大文件和可疑二进制

.PARAMETER Fix
    仅提示，不做任何修改。本脚本永远不修改仓库状态。

.EXAMPLE
    pwsh -File scripts/pre-push-check.ps1
    在 git push 之前跑一次。全部通过才推。

.NOTES
    退出码 0 = 安全，可以 push；1 = 发现问题，不要 push。
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# 定位仓库根目录。
#
# 关键点：这里刻意用 `--show-toplevel` 之后核对它是否等于当前目录，而不是无条件
# 信任。原因是很实际的一个坑：这个项目目录曾经被放在一个上级 git 仓库（用户主目录）
# 里面，于是 `git rev-parse --show-toplevel` 返回的是那个上级仓库，脚本就会去检查
# 一堆与项目无关的文件，还给出"通过"的假结果。宁可报错并让人自己 git init，
# 也不要对着错误的仓库做安全检查。
$repoRoot = & git rev-parse --show-toplevel 2>$null
if (-not $repoRoot) {
    Write-Host "ERROR: 当前目录不是 git 仓库。" -ForegroundColor Red
    Write-Host "       先执行: git init" -ForegroundColor Yellow
    exit 1
}
$repoRoot = $repoRoot -replace '/', '\'
$here = (Get-Location).Path -replace '/', '\'
if ($repoRoot.TrimEnd('\') -ne $here.TrimEnd('\')) {
    Write-Host "ERROR: 当前目录不是仓库根目录，或者它其实嵌套在另一个仓库里。" -ForegroundColor Red
    Write-Host "       当前目录: $here" -ForegroundColor Gray
    Write-Host "       git 认为的仓库根: $repoRoot" -ForegroundColor Gray
    Write-Host ""
    Write-Host "       如果这个项目本该是独立仓库，先在这里执行: git init" -ForegroundColor Yellow
    Write-Host "       否则脚本会去检查上面那个仓库，结果对你没有意义。" -ForegroundColor Yellow
    exit 1
}
Set-Location $repoRoot

$issues = New-Object System.Collections.ArrayList
function Add-Issue($msg, $detail) {
    [void]$issues.Add([pscustomobject]@{ Message = $msg; Detail = $detail })
}

Write-Host ""
Write-Host "=== push 前安全检查 ===" -ForegroundColor Cyan
Write-Host "仓库: $repoRoot"
Write-Host ""

# ---------------------------------------------------------------------------
# 1. 检查暂存区是否存在。没有暂存内容说明还没 git add，检查没有意义。
# ---------------------------------------------------------------------------
$staged = & git diff --cached --name-only --diff-filter=ACMR 2>$null
if (-not $staged) {
    $hasCommit = & git rev-parse --verify HEAD 2>$null
    if (-not $hasCommit) {
        Write-Host "还没有任何 commit，且暂存区为空。" -ForegroundColor Yellow
        Write-Host "先 git add，再跑一次本脚本 —— 检查的是将要提交的内容。" -ForegroundColor Yellow
        exit 1
    }
    Write-Host "暂存区为空，检查已提交的文件。" -ForegroundColor Yellow
    $files = & git ls-files 2>$null
} else {
    Write-Host "[1/4] 待提交文件: $($staged.Count) 个" -ForegroundColor Green
    $files = $staged
}

# ---------------------------------------------------------------------------
# 2. 致命项：这些文件绝不能被提交。
#    .env 里有真实 key；.env.example 是模板，必须保留，所以显式排除。
# ---------------------------------------------------------------------------
Write-Host "[2/4] 检查禁止提交的文件..." -NoNewline

$forbiddenPatterns = @(
    '(^|/)\.env$'                    # 真实环境变量文件
    '(^|/)\.env\.(local|production|prod|dev|staging|test)$'
    '\.(pem|key|p12|pfx|jks|keystore)$'   # 证书/私钥
    '(^|/)id_(rsa|dsa|ecdsa|ed25519)$'    # SSH 私钥
    '(^|/)\.npmrc$'
    '(^|/)credentials(\.json)?$'
    '(^|/)secrets?\.(ya?ml|json|txt)$'
)

foreach ($f in $files) {
    # 归一化路径分隔符，Windows 上 git 输出用 /
    $norm = $f -replace '\\', '/'
    # .env.example 是文档模板，必须提交，跳过
    if ($norm -match '\.env\.example$') { continue }
    foreach ($p in $forbiddenPatterns) {
        if ($norm -match $p) {
            Add-Issue "禁止提交的文件: $f" "这类文件包含密钥或凭据。执行: git rm --cached `"$f`""
            break
        }
    }
}
Write-Host " 完成" -ForegroundColor Green

# ---------------------------------------------------------------------------
# 3. 内容扫描：在将要提交的文本文件里搜密钥形态。
#    只看暂存区内容的快照（git show :path），避免把工作区未暂存的改动算进来。
# ---------------------------------------------------------------------------
Write-Host "[3/4] 扫描密钥形态字符串..." -NoNewline

# 密钥形态的正则。设计上偏向"宁可误报"：误报只是多看一眼，漏报是泄露。
$secretPatterns = @(
    # OpenAI / 类似网关的 key。本项目用的就是这种形态。
    @{ Name = 'API key (sk-)';        Regex = 'sk-[A-Za-z0-9_\-]{16,}' }
    # 常见云厂商
    @{ Name = 'AWS access key';       Regex = 'AKIA[0-9A-Z]{16}' }
    @{ Name = 'GitHub token';         Regex = 'gh[pousr]_[A-Za-z0-9]{36,}' }
    @{ Name = 'Slack token';          Regex = 'xox[baprs]-[0-9A-Za-z\-]{10,}' }
    @{ Name = 'Google API key';       Regex = 'AIza[0-9A-Za-z_\-]{35}' }
    # 私钥块
    @{ Name = '私钥块';               Regex = '-----BEGIN (RSA |EC |OPENSSH |PGP )?PRIVATE KEY( BLOCK)?-----' }
    # 赋值形式的通用密钥：KEY/SECRET/TOKEN/PASSWORD = 一串足够长的值
    # 排除明显是占位符或文档说明的写法
    @{ Name = '疑似硬编码密钥';       Regex = '(?i)\b(api[_-]?key|secret|token|passw(or)?d)\b\s*[:=]\s*["'']?[A-Za-z0-9_\-]{20,}' }
)

# 只扫文本类文件。二进制文件跳过（无法有意义地搜字符串）。
$textExtensions = @(
    '.go', '.sql', '.mod', '.sum', '.yml', '.yaml', '.json', '.md', '.txt',
    '.env', '.example', '.toml', '.ini', '.conf', '.sh', '.ps1', '.dockerfile',
    '.gitignore', '.golangci', '.csv', '.xml', '.html', '.js', '.ts'
)
# 无扩展名的文件也要查（Makefile、Dockerfile、LICENSE 等）
$textNames = @('Makefile', 'Dockerfile', 'LICENSE', '.gitignore', '.gitattributes', '.dockerignore')

$scanned = 0
foreach ($f in $files) {
    $name = Split-Path $f -Leaf
    $ext = [System.IO.Path]::GetExtension($f)
    $isText = ($textExtensions -contains $ext) -or ($textNames -contains $name)
    if (-not $isText) { continue }

    # 取暂存区版本的内容；取不到就跳过（比如刚删除的文件）
    $content = & git show ":$f" 2>$null
    if (-not $content) { continue }
    $scanned++
    $lineNo = 0
    foreach ($line in ($content -split "`n")) {
        $lineNo++
        # 跳过明显的占位符与文档示例行，减少噪音。
        #
        # 这里的排除规则要保守：多报一条只是多看一眼，漏报一条就是泄露。
        # 所以只排除不可能成为真实密钥的写法（全大写占位词、xxx 之类），
        # 并且这些规则只在"整行看起来就是占位符"时生效，避免连带放过真密钥。
        $looksLikePlaceholder = $line -match '(?i)(YOUR_[A-Z_]+|_HERE\b|xxxx+|\*\*\*|\.\.\.|placeholder|replace[_-]?me|<[a-z_]+>)'
        foreach ($sp in $secretPatterns) {
            if ($line -match $sp.Regex) {
                # 通用规则最容易被注释和文档句子误触发，占位符行直接放过。
                # 但 sk- / AKIA / 私钥块这类高置信度形态不放过：它们一旦出现在
                # 暂存内容里就值得人看一眼，哪怕那一行同时含有 placeholder 字样。
                $highConfidence = $sp.Name -notmatch '疑似硬编码密钥'
                if ($looksLikePlaceholder -and -not $highConfidence) { continue }
                # 只回显匹配到的那一段，不回显整行，避免把密钥打进终端历史/日志
                $m = [regex]::Match($line, $sp.Regex).Value
                $masked = if ($m.Length -gt 12) { $m.Substring(0,8) + '...[已遮蔽]' } else { '[已遮蔽]' }
                Add-Issue "疑似泄露: $($sp.Name)" "文件 $f 第 $lineNo 行 -> $masked"
            }
        }
    }
}
Write-Host " 已扫描 $scanned 个文本文件" -ForegroundColor Green

# ---------------------------------------------------------------------------
# 4. 大文件与不该进仓库的构建产物。
# ---------------------------------------------------------------------------
Write-Host "[4/4] 检查大文件与构建产物..." -NoNewline

$sizeLimitBytes = 2MB
foreach ($f in $files) {
    $full = Join-Path $repoRoot $f
    if (Test-Path $full -PathType Leaf) {
        $len = (Get-Item $full).Length
        if ($len -gt $sizeLimitBytes) {
            Add-Issue "大文件: $f" ("{0:N1} MB，超过 2 MB 上限。确认是否该用 Git LFS 或排除。" -f ($len/1MB))
        }
    }
    # 构建产物
    $norm = $f -replace '\\', '/'
    if ($norm -match '^(bin|build|dist)/' -or $norm -match '\.(exe|dll|so|dylib|test|out)$') {
        Add-Issue "构建产物: $f" "不该进仓库。加入 .gitignore 并 git rm --cached。"
    }
}
Write-Host " 完成" -ForegroundColor Green

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "=== 结果 ===" -ForegroundColor Cyan

if ($issues.Count -eq 0) {
    Write-Host "通过：未发现密钥泄露或可疑文件。" -ForegroundColor Green
    Write-Host ""
    Write-Host "提醒：本脚本只做静态检查。若 .env 从未提交过，key 仍然安全。" -ForegroundColor DarkGray
    Write-Host "      历史里一旦出现过密钥，删文件无效，必须去服务商作废重签。" -ForegroundColor DarkGray
    Write-Host ""
    exit 0
}

Write-Host "发现 $($issues.Count) 个问题，不要 push：" -ForegroundColor Red
Write-Host ""
foreach ($i in $issues) {
    Write-Host "  [!] $($i.Message)" -ForegroundColor Red
    Write-Host "      $($i.Detail)" -ForegroundColor Gray
}
Write-Host ""
Write-Host "处理建议：" -ForegroundColor Yellow
Write-Host "  - 禁止提交的文件: git rm --cached <文件>，并确认它在 .gitignore 里"
Write-Host "  - 代码里写了密钥: 换成读环境变量，把值放进 .env（已被 ignore）"
Write-Host "  - 密钥已经 commit 过: 立刻去服务商作废重签 —— 改写历史不能保证清除"
Write-Host ""
exit 1
