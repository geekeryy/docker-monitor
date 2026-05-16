#!/usr/bin/env bash
# ssh_brute_check.sh
# 用途: 分析 SSH 登录日志, 识别爆破来源 IP / 被尝试的用户名 / 攻击时段, 同时列出成功登录记录
#       并可周期性把"新事件"推送到钉钉机器人 (加签模式).
# 兼容: Debian/Ubuntu (auth.log), CentOS/RHEL/Rocky (secure), 以及任意带 systemd 的发行版 (journalctl)
# 用法:
#   ./ssh_brute_check.sh                  报告模式: 输出可读报告到终端
#   ./ssh_brute_check.sh --alert          告警模式: 增量分析 + 推钉钉 (cron 用)
#   ./ssh_brute_check.sh --json           JSON 输出, 便于二次处理
#   详见 -h
# sudo crontab -e
# */30 * * * * /path/to/ssh_brute_check.sh --alert >> /var/log/ssh_brute_alert.log 2>&1

set -eu
# 注意: 故意不开 pipefail. 多处使用 `... | head -n N`,
# head 提前关闭会让上游收到 SIGPIPE, 在 pipefail 下会被当作错误终止脚本.

# ============================================================
#                    >>> 用户配置区 <<<
# 仅在 "告警模式" (--alert) 下用到, 报告模式可以忽略.
#
# 安全建议:
#   - 修改 DT_SECRET 后 chmod 600 本脚本, 防止同机其他用户偷看
#   - 不要把填好真实 secret 的脚本提交到 git
# ============================================================

# --- 钉钉机器人 (告警模式必填, 加签) -------------------------
# 创建步骤: 钉钉群 -> 群设置 -> 智能群助手 -> 添加机器人 -> 自定义
# 安全设置勾选 "加签", 把 webhook 和签名密钥填到下面
DT_WEBHOOK=""
DT_SECRET=""
# --- 告警阈值 / 窗口 -----------------------------------------
ALERT_DAYS=1                    # 告警模式默认时间窗口 (天)
ALERT_FAIL_THRESHOLD=20         # 单 IP 失败次数 >= 该值才进入告警
ALERT_TOP_N=10                  # 告警里最多展示前 N 个攻击 IP
ALERT_DEDUP_HOURS=24            # 同一 IP / 同一高危事件 N 小时内不重复推送

# --- 告警类型开关 --------------------------------------------
ALERT_NOTIFY_NEW_SUCCESS=1      # 1=对新成功登录告警, 0=关闭
ALERT_NOTIFY_HIGH_RISK=1        # 1=对高危事件告警 (root 被试 / max_attempts), 0=关闭

# 受信任的成功登录 IP, 这些 IP 上的成功登录不告警 (空格分隔)
TRUSTED_SUCCESS_IPS=""

# --- 群通知 --------------------------------------------------
ALERT_AT_MOBILES=""             # @ 谁 (空格分隔的手机号), 留空表示不 @
ALERT_AT_ALL=0                  # 1=@所有人, 0=否 (慎用)

# --- 元信息 / 进阶 -------------------------------------------
HOST_LABEL=""                   # 告警标题里的机器名, 留空时取 hostname
STATE_DIR=""                    # 状态文件目录, 留空时取 /var/lib/ssh_brute_check

# ============================================================
#                  >>> 用户配置区结束 <<<
#                以下是脚本逻辑, 一般不需要改
# ============================================================

# ---------- 报告模式默认参数 ----------
DAYS=""                 # 分析最近 N 天 (留空: 报告模式默认 7, 告警模式默认 ALERT_DAYS)
TOP_N=""                # Top 榜显示条数 (留空: 报告模式默认 20, 告警模式默认 ALERT_TOP_N)
FILTER_IP=""            # 只看某个 IP
FILTER_USER=""          # 只看某个用户名
SHOW_RAW=0              # 是否输出原始命中日志
SHOW_SUCCESS=1          # 是否输出成功登录记录
SHOW_GEOIP=0            # 是否查 GeoIP (需要 geoiplookup)
JSON_OUTPUT=0           # JSON 输出
LOG_FILE=""             # 手动指定日志文件
USE_JOURNAL=""          # 强制使用 journalctl: 1=是, 0=否, 空=自动

# ---------- 告警模式额外开关 ----------
ALERT_MODE=0            # --alert
DRY_RUN=0               # --dry-run (告警模式下不真发)
RESET_STATE=0           # --reset-state (清空 state.json)
VERBOSE=0               # -v

# ---------- 颜色 ----------
if [[ -t 1 ]] && command -v tput >/dev/null 2>&1 && [[ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]]; then
    C_RED=$(tput setaf 1); C_GREEN=$(tput setaf 2); C_YELLOW=$(tput setaf 3)
    C_BLUE=$(tput setaf 4); C_MAGENTA=$(tput setaf 5); C_CYAN=$(tput setaf 6)
    C_BOLD=$(tput bold);    C_RESET=$(tput sgr0)
else
    C_RED=""; C_GREEN=""; C_YELLOW=""; C_BLUE=""; C_MAGENTA=""; C_CYAN=""; C_BOLD=""; C_RESET=""
fi

# ---------- 帮助 ----------
usage() {
    cat <<EOF
${C_BOLD}SSH 爆破检测 + 钉钉告警 (一体)${C_RESET}

用法:
  $0                           报告模式: 输出可读报告到终端 (默认窗口 7 天)
  $0 --alert                   告警模式: 增量分析 + 推钉钉   (默认窗口 ${ALERT_DAYS} 天)
  $0 --json                    JSON 输出, 便于二次处理

通用选项:
  -d, --days N         分析最近 N 天 (报告模式默认: 7, 告警模式默认: ${ALERT_DAYS})
  -n, --top N          每个 Top 榜显示条数 (报告模式默认: 20, 告警模式默认: ${ALERT_TOP_N})
  -i, --ip IP          只显示来自该 IP 的记录
  -u, --user USER      只显示尝试该用户名的记录
  -f, --file PATH      手动指定日志文件 (默认自动探测)
  -j, --journal        强制使用 journalctl 读取 sshd 日志
      --no-journal     强制不使用 journalctl, 使用文件方式
      --geoip          展示 IP 归属地 (需安装 geoiplookup), 报告/告警/JSON 均生效

报告模式选项:
      --raw            额外输出匹配到的原始日志行
      --no-success     不输出成功登录记录

告警模式选项:
      --alert          进入告警模式 (推钉钉, 增量去重)
      --dry-run        告警模式下只打印消息不真调钉钉 (调试用)
      --reset-state    清空状态文件 (下一次会全量推送, 慎用)
  -v, --verbose        告警模式下打印详细执行过程

  -h, --help           显示本帮助

示例:
  sudo $0                          # 报告模式, 最近 7 天
  sudo $0 -d 1 --geoip             # 报告模式, 1 天 + IP 归属地
  sudo $0 -i 1.2.3.4 --raw         # 查 1.2.3.4 的所有相关原始日志
  sudo $0 --json > out.json        # 导出 JSON 报告
  sudo $0 --alert                  # 告警模式 (cron 用)
  sudo $0 --alert --dry-run -v     # 告警模式调试 (不真发)

cron (每 30 分钟跑一次):
  */30 * * * * /path/to/$(basename "$0") --alert >> /var/log/ssh_brute_alert.log 2>&1

提示:
  - 多数发行版的 auth 日志只有 root 才能读, 通常需要 sudo 运行
  - 告警模式需要先编辑脚本顶部 "用户配置区" 填入钉钉 DT_WEBHOOK / DT_SECRET
EOF
}

# ---------- 解析参数 ----------
while [[ $# -gt 0 ]]; do
    case "$1" in
        -d|--days)        DAYS="$2"; shift 2 ;;
        -n|--top)         TOP_N="$2"; shift 2 ;;
        -i|--ip)          FILTER_IP="$2"; shift 2 ;;
        -u|--user)        FILTER_USER="$2"; shift 2 ;;
        -f|--file)        LOG_FILE="$2"; shift 2 ;;
        -j|--journal)     USE_JOURNAL=1; shift ;;
        --no-journal)     USE_JOURNAL=0; shift ;;
        --raw)            SHOW_RAW=1; shift ;;
        --no-success)     SHOW_SUCCESS=0; shift ;;
        --geoip)          SHOW_GEOIP=1; shift ;;
        --json)           JSON_OUTPUT=1; shift ;;
        --alert)          ALERT_MODE=1; shift ;;
        --dry-run)        DRY_RUN=1; shift ;;
        --reset-state)    RESET_STATE=1; shift ;;
        -v|--verbose)     VERBOSE=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        *) echo "未知参数: $1" >&2; usage; exit 1 ;;
    esac
done

# ---------- 解析后处理: 模式相关默认值 ----------
if [[ $ALERT_MODE -eq 1 ]]; then
    DAYS="${DAYS:-$ALERT_DAYS}"
    TOP_N="${TOP_N:-$ALERT_TOP_N}"
    JSON_OUTPUT=1   # 告警模式内部强制走 JSON 路径, 用于增量计算
    SHOW_RAW=0      # 告警模式不输出原始日志

    # 提前校验钉钉配置 (fail fast), 避免出现 "找不到日志源" 这种误导性错误
    case "$DT_WEBHOOK" in
        TODO|TODO_*|REPLACE_ME_*|"")
            echo "错误: DT_WEBHOOK 还是占位符, 请先编辑脚本顶部 '用户配置区' 填入真实 webhook" >&2
            exit 1 ;;
    esac
    case "$DT_SECRET" in
        TODO|TODO_*|REPLACE_ME_*|"")
            echo "错误: DT_SECRET 还是占位符, 请先编辑脚本顶部 '用户配置区' 填入签名密钥" >&2
            exit 1 ;;
    esac
else
    DAYS="${DAYS:-7}"
    TOP_N="${TOP_N:-20}"
fi

# ---------- 工具函数 ----------
log_info()  { [[ $JSON_OUTPUT -eq 1 ]] || echo "${C_CYAN}[INFO]${C_RESET}  $*" >&2; }
log_warn()  { [[ $JSON_OUTPUT -eq 1 ]] || echo "${C_YELLOW}[WARN]${C_RESET}  $*" >&2; }
log_error() { echo "${C_RED}[ERROR]${C_RESET} $*" >&2; }

require_int() {
    local name="$1" val="$2"
    if ! [[ "$val" =~ ^[0-9]+$ ]] || [[ "$val" -le 0 ]]; then
        log_error "$name 必须是正整数, 当前值: $val"; exit 1
    fi
}
require_int "--days" "$DAYS"
require_int "--top"  "$TOP_N"

# ---------- 选择日志源 ----------
detect_source() {
    if [[ -n "$LOG_FILE" ]]; then
        [[ -r "$LOG_FILE" ]] || { log_error "无法读取日志文件: $LOG_FILE (可能需要 sudo)"; exit 1; }
        echo "file:$LOG_FILE"; return
    fi

    if [[ "$USE_JOURNAL" == "1" ]]; then
        command -v journalctl >/dev/null 2>&1 || { log_error "未找到 journalctl"; exit 1; }
        echo "journal"; return
    fi

    if [[ "$USE_JOURNAL" == "0" ]]; then
        for f in /var/log/auth.log /var/log/secure; do
            [[ -r "$f" ]] && { echo "file:$f"; return; }
        done
        log_error "未找到可读的 auth 日志, 请用 -f 指定或加 sudo"; exit 1
    fi

    # 自动: 优先 journalctl (覆盖更全, 含被 logrotate 压缩的部分)
    if command -v journalctl >/dev/null 2>&1; then
        if journalctl -u ssh -u sshd -n 1 >/dev/null 2>&1; then
            echo "journal"; return
        fi
    fi
    for f in /var/log/auth.log /var/log/secure; do
        [[ -r "$f" ]] && { echo "file:$f"; return; }
    done
    log_error "未找到可读的 SSH 日志源, 请用 -f 指定文件路径或加 sudo"; exit 1
}

SOURCE="$(detect_source)"
log_info "日志源: ${C_BOLD}${SOURCE}${C_RESET}"
log_info "分析范围: 最近 ${C_BOLD}${DAYS}${C_RESET} 天"

# ---------- 取出原始日志 ----------
# 输出统一为以日志行为单位, 带时间前缀 (journalctl 用 --output=short-iso 让时间可解析)
fetch_logs() {
    case "$SOURCE" in
        journal)
            journalctl -u ssh -u sshd \
                --since "${DAYS} days ago" \
                --output=short-iso --no-pager 2>/dev/null
            ;;
        file:*)
            local path="${SOURCE#file:}"
            local since_ts
            since_ts=$(date -d "${DAYS} days ago" +%s 2>/dev/null || \
                       python3 -c "import time; print(int(time.time()-${DAYS}*86400))")
            # 同时读取 path 和它的 logrotate 历史 (path.1, path.2.gz, ...)
            local files=("$path")
            for ext in 1 2 3 4 5 6 7; do
                [[ -r "${path}.${ext}"    ]] && files+=("${path}.${ext}")
                [[ -r "${path}.${ext}.gz" ]] && files+=("${path}.${ext}.gz")
            done
            # 按时间倒着读, 尽量保证后面 awk 能拿到完整窗口
            local cur_year
            cur_year=$(date +%Y)
            for f in "${files[@]}"; do
                if [[ "$f" == *.gz ]]; then zcat -- "$f"; else cat -- "$f"; fi
            done | awk -v since="$since_ts" -v cur_year="$cur_year" '
                # syslog 格式时间戳没有年份, 这里粗略按当前年份做时间过滤;
                # 跨年场景会有边界误差, 但对最近 N 天的分析足够准.
                BEGIN {
                    split("Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec", m, " ");
                    for (i=1; i<=12; i++) mon[m[i]] = sprintf("%02d", i);
                }
                {
                    # 兼容 RFC3339 (2026-04-23T10:00:00) 与 syslog (Apr 23 10:00:00) 两种前缀
                    if ($1 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/) {
                        ts_str = $1;
                        gsub("T", " ", ts_str);
                        sub(/[+-][0-9:]+$/, "", ts_str);
                    } else if ($1 in mon) {
                        ts_str = cur_year "-" mon[$1] "-" sprintf("%02d", $2) " " $3;
                    } else { next }
                    # 同一秒可能有多条日志, 加缓存避免反复 fork date
                    if (!(ts_str in ts_cache)) {
                        cmd = "date -d \"" ts_str "\" +%s 2>/dev/null";
                        cmd | getline t; close(cmd);
                        ts_cache[ts_str] = t;
                    }
                    if (ts_cache[ts_str] >= since) print ts_cache[ts_str] "\t" $0;
                }' | sort -n | cut -f2-
            ;;
    esac
}

# ---------- 临时文件 ----------
TMP_DIR=$(mktemp -d -t sshcheck.XXXXXX)
trap 'rm -rf "$TMP_DIR"' EXIT

RAW="$TMP_DIR/raw.log"
fetch_logs > "$RAW" || true

if [[ ! -s "$RAW" ]]; then
    log_warn "未读取到任何日志, 请确认日志源/权限/天数"
    [[ $JSON_OUTPUT -eq 1 ]] && echo '{"failed_total":0,"success_total":0}'
    exit 0
fi

# ---------- 提取关键信息 ----------
# 失败登录: 命中常见 sshd 失败模式, 提取 (时间, 用户名, IP, 失败原因)
FAIL_TSV="$TMP_DIR/failed.tsv"

awk -v ip_filter="$FILTER_IP" -v user_filter="$FILTER_USER" '
    # ---- 守卫 1: 只处理 sshd 进程的日志行 ----
    # /var/log/auth.log 里同时混合了 sudo / su / login / polkitd / cron(pam) 等服务的 PAM 失败,
    # 它们的日志格式跟 sshd 高度相似 ("authentication failure; ... rhost= user=xxx"),
    # 不过滤就会把本地 sudo 输错密码的用户(比如系统里有个 "ps" 用户) 误判为 SSH 爆破尝试.
    # 兼容 sshd[123]: / sshd-session[123]: / sshd-auth[123]: 等 (OpenSSH 9.8+ 拆分进程后会出现).
    $0 !~ /sshd(-[a-z]+)?\[[0-9]+\]:/ { next }

    function emit(reason, user, ip,    ts) {
        if (ip == "" ) return;
        if (ip_filter   != "" && ip   != ip_filter)   return;
        if (user_filter != "" && user != user_filter) return;

        # 取行首时间戳作为 ts (journalctl --output=short-iso 给的就是 ISO; 文件日志是 syslog 格式)
        ts = $1 " " $2 " " $3;
        if ($1 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/) ts = $1;
        gsub(/\t/, " ", ts);
        printf "%s\t%s\t%s\t%s\n", ts, (user==""?"-":user), ip, reason;
    }

    # 守卫 2: 找到 sshd 报头位置, 后面才是真正的消息体, 字段扫描从这之后开始.
    # 这样可以避免主机名 / sshd[pid] / 时间戳里偶然出现的 "user" "for" "from" 字面被误识别.
    function msg_start(    j) {
        for (j=1; j<=NF; j++) if ($j ~ /sshd(-[a-z]+)?\[[0-9]+\]:/) return j+1;
        return 1;
    }

    # 1) Failed password for invalid user xxx from 1.2.3.4 port 5678 ssh2
    /Failed password for invalid user/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="user") { user=$(i+1); break }
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        emit("invalid_user", user, ip); next;
    }
    # 2) Failed password for xxx from 1.2.3.4 port 5678 ssh2 (用户存在但密码错)
    /Failed password for/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="for")  { user=$(i+1); break }
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        emit("wrong_password", user, ip); next;
    }
    # 3) Invalid user xxx from 1.2.3.4 port 5678
    /Invalid user/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="user") { user=$(i+1); break }
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        emit("invalid_user", user, ip); next;
    }
    # 4) Disconnected from authenticating user xxx 1.2.3.4 port 5678 [preauth]
    #    Disconnected from invalid user xxx 1.2.3.4 port 5678 [preauth]   (OpenSSH 7.x+)
    /Disconnected from (authenticating|invalid) user/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="user") { user=$(i+1); ip=$(i+2); break }
        reason = ($0 ~ /invalid user/) ? "invalid_user" : "preauth_disconnect";
        emit(reason, user, ip); next;
    }
    # 5) Connection closed by invalid user xxx 1.2.3.4 port 5678 [preauth]
    /Connection closed by invalid user/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="user") { user=$(i+1); ip=$(i+2); break }
        emit("invalid_user", user, ip); next;
    }
    # 6) Connection closed by authenticating user xxx 1.2.3.4 port 5678 [preauth]
    /Connection closed by authenticating user/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) if ($i=="user") { user=$(i+1); ip=$(i+2); break }
        emit("preauth_disconnect", user, ip); next;
    }
    # 7) Did not receive identification string from 1.2.3.4 (扫描器探测端口)
    /Did not receive identification string from/ {
        s=msg_start(); ip="";
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        emit("port_scan", "-", ip); next;
    }
    # 8) error: maximum authentication attempts exceeded for [invalid user] xxx from 1.2.3.4 port 5678 ssh2 [preauth]
    /maximum authentication attempts exceeded for/ {
        s=msg_start(); user=""; ip=""; last_for=0;
        # 取最后一个 "for" 后面的 token 作为用户名 (兼容 "exceeded for invalid user xxx" 与 "exceeded for root")
        for (i=s;i<=NF;i++) if ($i=="for") last_for=i;
        if (last_for) {
            user = ($(last_for+1)=="invalid" && $(last_for+2)=="user") ? $(last_for+3) : $(last_for+1);
        }
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        emit("max_attempts", user, ip); next;
    }
    # 9) Failed publickey/none/keyboard-interactive for [invalid user] xxx from 1.2.3.4 port 5678 ssh2
    /Failed (publickey|none|keyboard-interactive) for/ {
        s=msg_start(); user=""; ip=""; last_for=0;
        for (i=s;i<=NF;i++) if ($i=="for") last_for=i;
        if (last_for) {
            user = ($(last_for+1)=="invalid" && $(last_for+2)=="user") ? $(last_for+3) : $(last_for+1);
        }
        for (i=s;i<=NF;i++) if ($i=="from") { ip=$(i+1); break }
        reason = ($0 ~ /invalid user/) ? "invalid_user" : "wrong_credential";
        emit(reason, user, ip); next;
    }
    # 10) sshd 自身的 PAM 失败 (rhost= 必须为非空 IP, 排除本地 PAM 噪声)
    /pam_unix\(sshd:/ && /authentication failure/ && /rhost=/ {
        s=msg_start(); user=""; ip="";
        for (i=s;i<=NF;i++) {
            if ($i ~ /^rhost=/) { ip   = substr($i, 7) }
            if ($i ~ /^user=/)  { user = substr($i, 6) }
        }
        # rhost= 后面为空 (本地登录) 的就别算成 SSH 爆破了
        if (ip == "") next;
        emit("pam_failure", user, ip); next;
    }
' "$RAW" > "$FAIL_TSV"

# 成功登录: Accepted password/publickey/keyboard-interactive/hostbased for xxx from 1.2.3.4 port 5678
SUCC_TSV="$TMP_DIR/success.tsv"
awk -v ip_filter="$FILTER_IP" -v user_filter="$FILTER_USER" '
    # 同样的 sshd 守卫: 排除 sudo / login 等同名关键字的干扰
    $0 !~ /sshd(-[a-z]+)?\[[0-9]+\]:/ { next }

    /Accepted (password|publickey|keyboard-interactive|hostbased|gssapi-with-mic)/ {
        ts = $1 " " $2 " " $3;
        if ($1 ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/) ts = $1;
        method=""; user=""; ip="";
        for (i=1;i<=NF;i++) {
            if ($i=="Accepted") method=$(i+1);
            if ($i=="for")      user=$(i+1);
            if ($i=="from")     ip=$(i+1);
        }
        if (ip_filter   != "" && ip   != ip_filter)   next;
        if (user_filter != "" && user != user_filter) next;
        printf "%s\t%s\t%s\t%s\n", ts, (user==""?"-":user), ip, method;
    }
' "$RAW" > "$SUCC_TSV"

FAIL_TOTAL=$(wc -l < "$FAIL_TSV" | tr -d ' ')
SUCC_TOTAL=$(wc -l < "$SUCC_TSV" | tr -d ' ')

# ---------- GeoIP ----------
geoip_of() {
    local ip="$1"
    if [[ $SHOW_GEOIP -eq 1 ]] && command -v geoiplookup >/dev/null 2>&1; then
        geoiplookup "$ip" 2>/dev/null | head -n1 | sed 's/^GeoIP[^:]*: //'
    else
        echo ""
    fi
}

# ---------- JSON 构造 (供 --json / --alert 两种用途共享) ----------
build_report_json() {
    local top_attack_ips top_users privileged_user_failures reason_distribution successful_logins

    top_attack_ips=$(
        awk -F'\t' '{cnt[$3]++} END{for (ip in cnt) print cnt[ip] "\t" ip}' "$FAIL_TSV" |
        sort -t $'\t' -k1,1rn | head -n "$TOP_N" |
        awk -F'\t' '{print $2 "\t" $1}' |
        jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {ip: .[0], count: (.[1] | tonumber)})'
    )

    top_users=$(
        awk -F'\t' '$2 != "-" {cnt[$2]++} END{for (user in cnt) print cnt[user] "\t" user}' "$FAIL_TSV" |
        sort -t $'\t' -k1,1rn | head -n "$TOP_N" |
        awk -F'\t' '{print $2 "\t" $1}' |
        jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {user: .[0], count: (.[1] | tonumber)})'
    )

    privileged_user_failures=$(
        awk -F'\t' '$2 == "root" || $2 == "admin" {cnt[$2]++} END{for (user in cnt) print user "\t" cnt[user]}' "$FAIL_TSV" |
        sort -t $'\t' -k2,2rn |
        jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {user: .[0], count: (.[1] | tonumber)})'
    )

    reason_distribution=$(
        awk -F'\t' '{cnt[$4]++} END{for (reason in cnt) print cnt[reason] "\t" reason}' "$FAIL_TSV" |
        sort -t $'\t' -k1,1rn |
        awk -F'\t' '{print $2 "\t" $1}' |
        jq -R -s 'split("\n") | map(select(length > 0) | split("\t") | {reason: .[0], count: (.[1] | tonumber)})'
    )

    successful_logins=$(
        jq -R -s '
            split("\n") |
            map(select(length > 0) | split("\t") | select(length >= 4) |
                {time: .[0], user: .[1], ip: .[2], method: .[3]})
        ' "$SUCC_TSV"
    )

    jq -n \
        --arg source "$SOURCE" \
        --argjson days "$DAYS" \
        --argjson failed_total "$FAIL_TOTAL" \
        --argjson success_total "$SUCC_TOTAL" \
        --argjson top_attack_ips "$top_attack_ips" \
        --argjson top_users "$top_users" \
        --argjson privileged_user_failures "$privileged_user_failures" \
        --argjson reason_distribution "$reason_distribution" \
        --argjson successful_logins "$successful_logins" '
        {
            source: $source,
            days: $days,
            failed_total: $failed_total,
            success_total: $success_total,
            top_attack_ips: $top_attack_ips,
            top_users: $top_users,
            privileged_user_failures: $privileged_user_failures,
            reason_distribution: $reason_distribution,
            successful_logins: $successful_logins
        }
    '
}

# ---------- 告警模式分支 ----------
if [[ $ALERT_MODE -eq 1 ]]; then
    # 配置占位符已在解析参数后校验过 (fail fast), 这里直接进入业务逻辑

    # === 1. 兜底默认 + 依赖检查 ===
    HOST_LABEL="${HOST_LABEL:-$(hostname)}"
    STATE_DIR="${STATE_DIR:-/var/lib/ssh_brute_check}"
    STATE_FILE="$STATE_DIR/state.json"
    for cmd in curl jq openssl base64; do
        command -v "$cmd" >/dev/null 2>&1 || {
            echo "缺少依赖: $cmd. 通常: apt install -y curl jq openssl   或   yum install -y curl jq openssl" >&2
            exit 1
        }
    done

    alog()  { [[ $VERBOSE -eq 1 ]] && echo "[$(date '+%F %T')] $*" >&2; return 0; }
    afail() { echo "[$(date '+%F %T')] [ERROR] $*" >&2; exit 1; }

    # === 3. 初始化 state ===
    mkdir -p "$STATE_DIR"
    if [[ $RESET_STATE -eq 1 ]] || [[ ! -s "$STATE_FILE" ]]; then
        echo '{"version":1,"alerted_ips":{},"known_success":[],"high_risk_seen":{}}' > "$STATE_FILE"
        alog "状态文件已重置: $STATE_FILE"
    fi

    # === 4. 拿到当前窗口的报告 JSON ===
    REPORT="$(build_report_json)"
    echo "$REPORT" | jq -e . >/dev/null 2>&1 || afail "JSON 构造失败 (内部 bug)"

    NOW_TS=$(date +%s)
    NOW_ISO=$(date '+%F %T %Z')

    # 读出旧 state
    ALERTED_IPS_JSON=$(jq -c '.alerted_ips // {}'  "$STATE_FILE")
    KNOWN_SUCCESS_JSON=$(jq -c '.known_success // []' "$STATE_FILE")
    HIGH_RISK_SEEN_JSON=$(jq -c '.high_risk_seen // {}' "$STATE_FILE")

    # 受信任 IP → JSON array
    # shellcheck disable=SC2086
    TRUSTED_IPS_JSON=$(printf '%s\n' ${TRUSTED_SUCCESS_IPS:-} | jq -R . | jq -sc 'map(select(. != ""))')

    # === 5. 算增量 ===
    # 5.1 新攻击 IP (含去重)
    NEW_ATTACK_IPS=$(jq -c \
        --argjson threshold "$ALERT_FAIL_THRESHOLD" \
        --argjson alerted "$ALERTED_IPS_JSON" \
        --argjson dedup_h "$ALERT_DEDUP_HOURS" \
        --argjson now_ts "$NOW_TS" \
        --argjson topn "$TOP_N" '
        .top_attack_ips // [] |
        map(select(.count >= $threshold)) |
        map(select(
            (($alerted[.ip] // 0) | tonumber) as $last_ts |
            ($now_ts - $last_ts) > ($dedup_h * 3600)
        )) |
        .[0:$topn]
    ' <<< "$REPORT")

    # 5.2 新成功登录 (按 time|user|ip|method 去重, 排除受信任 IP)
    NEW_SUCCESS=$(jq -c \
        --argjson known "$KNOWN_SUCCESS_JSON" \
        --argjson trusted "$TRUSTED_IPS_JSON" '
        (.successful_logins // []) |
        map(. + {key: (.time + "|" + .user + "|" + .ip + "|" + .method)}) |
        map(. as $login | select(
            (($known | index($login.key)) == null)
            and (($trusted | index($login.ip)) == null)
        ))
    ' <<< "$REPORT")
    CURRENT_SUCCESS_KEYS=$(jq -c '
        [(.successful_logins // [])[] | .time + "|" + .user + "|" + .ip + "|" + .method]
    ' <<< "$REPORT")

    # 5.3 高危事件 (root/admin 被试 / max_attempts), 按指纹去重
    HIGH_RISK_RAW=$(jq -c '
        {
            root_wrong_pw: (.privileged_user_failures // []),
            max_attempts_count: (
                (.reason_distribution // []) |
                map(select(.reason == "max_attempts")) |
                (if length == 0 then 0 else .[0].count end)
            )
        }
    ' <<< "$REPORT")
    HIGH_RISK=$(jq -c \
        --argjson seen "$HIGH_RISK_SEEN_JSON" \
        --argjson dedup_h "$ALERT_DEDUP_HOURS" \
        --argjson now_ts "$NOW_TS" '
        def fresh($key): (($seen[$key] // 0 | tonumber) as $last | ($now_ts - $last) > ($dedup_h * 3600));
        {
            root_wrong_pw: (.root_wrong_pw | map(select(fresh("root_wrong_pw:" + .user)))),
            max_attempts_count: (if (.max_attempts_count > 0 and (fresh("max_attempts"))) then .max_attempts_count else 0 end)
        }
    ' <<< "$HIGH_RISK_RAW")

    n_new_ips=$(jq 'length' <<< "$NEW_ATTACK_IPS")
    n_new_succ=$(jq 'length' <<< "$NEW_SUCCESS")
    n_max_atp=$(jq '.max_attempts_count' <<< "$HIGH_RISK")
    has_root_wpw=$(jq '.root_wrong_pw | length' <<< "$HIGH_RISK")

    alog "增量统计: 新攻击IP=$n_new_ips 新成功登录=$n_new_succ root/admin相关=$has_root_wpw max_attempts=$n_max_atp"

    [[ $ALERT_NOTIFY_NEW_SUCCESS -eq 0 ]] && { NEW_SUCCESS='[]'; n_new_succ=0; }
    if [[ $ALERT_NOTIFY_HIGH_RISK -eq 0 ]]; then
        HIGH_RISK='{"root_wrong_pw":[],"max_attempts_count":0}'
        has_root_wpw=0; n_max_atp=0
    fi

    if [[ $n_new_ips -eq 0 ]] && [[ $n_new_succ -eq 0 ]] && [[ $has_root_wpw -eq 0 ]] && [[ $n_max_atp -eq 0 ]]; then
        alog "没有新事件, 静默退出"
        exit 0
    fi

    # === 6. 给攻击 IP 附加 GeoIP (如果开了 --geoip) ===
    if [[ $SHOW_GEOIP -eq 1 ]] && command -v geoiplookup >/dev/null 2>&1; then
        NEW_ATTACK_IPS=$(echo "$NEW_ATTACK_IPS" | jq -c '.[]' |
            while IFS= read -r row; do
                ip=$(echo "$row" | jq -r '.ip')
                geo=$(geoiplookup "$ip" 2>/dev/null | head -n1 | sed 's/^GeoIP[^:]*: //')
                echo "$row" | jq -c --arg geo "$geo" '. + {geo: $geo}'
            done | jq -sc .)
    fi

    # === 7. 构造 markdown ===
    TITLE_TAG=""
    [[ $n_new_succ -gt 0 ]]    && TITLE_TAG="${TITLE_TAG}🚨"
    [[ $has_root_wpw -gt 0 ]]  && TITLE_TAG="${TITLE_TAG}⚠️"
    TITLE_TAG="${TITLE_TAG:-🛡️}"
    TITLE="[SSH告警] ${TITLE_TAG} ${HOST_LABEL}"

    SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

    TEXT=$(jq -nr \
        --arg host       "$HOST_LABEL" \
        --arg now        "$NOW_ISO" \
        --arg days       "$DAYS" \
        --arg threshold  "$ALERT_FAIL_THRESHOLD" \
        --arg script     "$SCRIPT_PATH" \
        --argjson topn        "$TOP_N" \
        --argjson new_ips     "$NEW_ATTACK_IPS" \
        --argjson new_success "$NEW_SUCCESS" \
        --argjson high_risk   "$HIGH_RISK" \
        --argjson report      "$REPORT" '
        def line(s): s + "\n";
        def section(s): "\n#### " + s + "\n";
        def login_line: "- 时间: `\(.time)`  来源 IP: `\(.ip)`  用户: **\(.user)**  方式: \(.method)";

        line("### SSH 安全告警 — \($host)")
        + line("> 时间: \($now)  /  扫描窗口: 最近 \($days) 天  /  阈值: \($threshold) 次")
        + line("> 失败总数: **\($report.failed_total // 0)**, 成功登录: **\($report.success_total // 0)**")

        + ( if ($high_risk.root_wrong_pw | length) > 0 then
              section("⚠️ 高危: 特权账号被尝试密码")
              + ( ($high_risk.root_wrong_pw | map("- **\(.user)**: 失败 \(.count) 次") | join("\n")) + "\n" )
            else "" end )

        + ( if $high_risk.max_attempts_count > 0 then
              section("⚠️ 高危: 触发最大认证次数限制")
              + line("- 共 **\($high_risk.max_attempts_count)** 次, 典型爆破特征")
            else "" end )

        + ( if ($new_success | length) > 0 then
              section("🚨 新成功登录事件")
              + ( $new_success | map(login_line) | join("\n") ) + "\n"
            else "" end )

        + ( if (($new_success | length) == 0 and (($report.successful_logins // []) | length) > 0) then
              section("成功登录明细 (最近 \([(($report.successful_logins // []) | length), $topn] | min) 条)")
              + ( ($report.successful_logins // []) | reverse | .[0:$topn] | map(login_line) | join("\n") ) + "\n"
            else "" end )

        + ( if ($new_ips | length) > 0 then
              section("🛡️ 新增高频攻击 IP (Top \($new_ips | length))")
              + ( $new_ips | map(
                  if (.geo // "") != "" then
                      "- `\(.ip)` — 失败 **\(.count)** 次  _\(.geo)_"
                  else
                      "- `\(.ip)` — 失败 **\(.count)** 次"
                  end
                ) | join("\n") ) + "\n"
            else "" end )

        + line("\n---")
        + line("- 完整报告: `sudo \($script) -d \($days)`")
        + line("- 想查某个 IP 原始日志: `sudo \($script) -i <IP> --raw`")
    ')

    # === 8. 加签 + 发送 ===
    sign_dingtalk() {
        local secret="$1" ts_ms="$2"
        local string_to_sign sign sign_url
        # 钉钉规定 string_to_sign = "{timestamp}\n{secret}", 中间是真换行符
        string_to_sign="${ts_ms}
${secret}"
        sign=$(printf '%s' "$string_to_sign" | openssl dgst -sha256 -hmac "$secret" -binary | base64)
        sign_url=$(jq -nr --arg s "$sign" '$s|@uri')
        echo "${sign_url}"
    }

    TS_MS=$(($(date +%s) * 1000))
    SIGN=$(sign_dingtalk "$DT_SECRET" "$TS_MS")
    URL="${DT_WEBHOOK}&timestamp=${TS_MS}&sign=${SIGN}"

    # shellcheck disable=SC2086
    AT_MOBILES_JSON=$(printf '%s\n' ${ALERT_AT_MOBILES:-} | jq -R . | jq -sc 'map(select(. != ""))')
    [[ $ALERT_AT_ALL -eq 1 || "$ALERT_AT_ALL" == "true" ]] && AT_ALL=true || AT_ALL=false

    PAYLOAD=$(jq -nc \
        --arg title    "$TITLE" \
        --arg text     "$TEXT" \
        --argjson mobs "$AT_MOBILES_JSON" \
        --argjson all  "$AT_ALL" '
        {
            msgtype: "markdown",
            markdown: { title: $title, text: $text },
            at: { atMobiles: $mobs, isAtAll: $all }
        }')

    if [[ $DRY_RUN -eq 1 ]]; then
        echo "===== DRY RUN: 不真发钉钉, 以下是消息 ====="
        echo "URL: ${DT_WEBHOOK}&timestamp=${TS_MS}&sign=<签名隐藏>"
        echo "----- payload -----"
        echo "$PAYLOAD" | jq .
        echo "----- 渲染后的 markdown -----"
        echo "$TEXT"
        echo "============================================"
        alog "dry-run 模式: 仍然更新 state 以便测试去重"
    else
        alog "POST 钉钉..."
        RESP=$(curl -sS -m 10 -X POST -H 'Content-Type: application/json' -d "$PAYLOAD" "$URL" || echo '{}')
        RESP_CODE=$(echo "$RESP" | jq -r '.errcode // -1')
        RESP_MSG=$(echo "$RESP" | jq -r '.errmsg // "no response"')
        if [[ "$RESP_CODE" != "0" ]]; then
            afail "钉钉返回失败: errcode=$RESP_CODE errmsg=$RESP_MSG resp=$RESP"
        fi
        alog "钉钉发送成功"
    fi

    # === 9. 更新 state ===
    NEW_ALERTED_IPS=$(jq -c --argjson now "$NOW_TS" '
        map({key: .ip, value: $now}) | from_entries
    ' <<< "$NEW_ATTACK_IPS")
    NEW_SUCC_KEYS=$(jq -c '[.[].key]' <<< "$NEW_SUCCESS")
    NEW_HIGH_RISK_SEEN=$(jq -c --argjson now "$NOW_TS" '
        [
            (.root_wrong_pw[]?  | {key: ("root_wrong_pw:" + .user), value: $now}),
            (if .max_attempts_count > 0 then {key: "max_attempts", value: $now} else empty end)
        ] | from_entries
    ' <<< "$HIGH_RISK")

    UPDATED=$(jq -c \
        --argjson new_alerted   "$NEW_ALERTED_IPS" \
        --argjson new_succ_keys "$NEW_SUCC_KEYS" \
        --argjson current_succ_keys "$CURRENT_SUCCESS_KEYS" \
        --argjson new_hr_seen   "$NEW_HIGH_RISK_SEEN" '
        .alerted_ips    = ((.alerted_ips    // {}) + $new_alerted)        |
        .known_success  = (
            (.known_success // []) + $new_succ_keys |
            unique |
            map(. as $key | select(($current_succ_keys | index($key)) != null))
        ) |
        .high_risk_seen = ((.high_risk_seen // {}) + $new_hr_seen)
    ' "$STATE_FILE")
    echo "$UPDATED" > "$STATE_FILE.tmp" && mv "$STATE_FILE.tmp" "$STATE_FILE"
    alog "状态文件已更新: $STATE_FILE"

    exit 0
fi

# ---------- JSON 输出分支 ----------
if [[ $JSON_OUTPUT -eq 1 ]]; then
    command -v jq >/dev/null 2>&1 || {
        echo "缺少依赖: jq. 通常: apt install -y jq   或   yum install -y jq" >&2
        exit 1
    }
    build_report_json
    exit 0
fi

# ---------- 文本报告输出 ----------
echo
echo "${C_BOLD}===================== SSH 安全态势报告 =====================${C_RESET}"
echo "  日志源        : $SOURCE"
echo "  时间范围      : 最近 ${DAYS} 天"
echo "  失败尝试总数  : ${C_RED}${FAIL_TOTAL}${C_RESET}"
echo "  成功登录次数  : ${C_GREEN}${SUCC_TOTAL}${C_RESET}"
[[ -n "$FILTER_IP"   ]] && echo "  IP 过滤       : $FILTER_IP"
[[ -n "$FILTER_USER" ]] && echo "  用户过滤      : $FILTER_USER"
echo "${C_BOLD}============================================================${C_RESET}"

print_section() {
    echo
    echo "${C_BOLD}${C_BLUE}>> $1${C_RESET}"
}

# ---- 1. Top 攻击 IP ----
print_section "Top ${TOP_N} 攻击来源 IP (按失败次数)"
if [[ "$FAIL_TOTAL" -eq 0 ]]; then
    echo "  (无失败登录记录)"
else
    printf "  ${C_BOLD}%-6s  %-18s  %-10s  %s${C_RESET}\n" "次数" "IP 地址" "尝试用户数" "归属 (GeoIP)"
    awk -F'\t' '{print $3"\t"$2}' "$FAIL_TSV" | sort -u |
        awk -F'\t' '{cnt[$1]++} END{for (i in cnt) print cnt[i]"\t"i}' > "$TMP_DIR/ip_user_uniq.tsv"
    awk -F'\t' '{print $3}' "$FAIL_TSV" | sort | uniq -c | sort -rn | head -n "$TOP_N" |
    while read -r cnt ip; do
        users_cnt=$(awk -F'\t' -v ip="$ip" '$2==ip {print $1; exit}' "$TMP_DIR/ip_user_uniq.tsv")
        users_cnt=${users_cnt:-0}
        geo="$(geoip_of "$ip")"
        printf "  ${C_RED}%-6s${C_RESET}  %-18s  %-10s  %s\n" "$cnt" "$ip" "$users_cnt" "$geo"
    done
fi

# ---- 2. Top 被尝试用户名 ----
print_section "Top ${TOP_N} 被尝试用户名"
if [[ "$FAIL_TOTAL" -eq 0 ]]; then
    echo "  (无失败登录记录)"
else
    printf "  ${C_BOLD}%-6s  %s${C_RESET}\n" "次数" "用户名"
    awk -F'\t' '$2 != "-" {print $2}' "$FAIL_TSV" | sort | uniq -c | sort -rn | head -n "$TOP_N" |
    while read -r cnt user; do
        printf "  ${C_YELLOW}%-6s${C_RESET}  %s\n" "$cnt" "$user"
    done
fi

# ---- 3. 失败原因分布 ----
print_section "失败原因分布"
awk -F'\t' '{print $4}' "$FAIL_TSV" | sort | uniq -c | sort -rn |
while read -r cnt reason; do
    case "$reason" in
        invalid_user)        desc="不存在的用户 (爆破尝试)" ;;
        wrong_password)      desc="密码错误 (爆破尝试)" ;;
        wrong_credential)    desc="密钥/键盘交互认证失败" ;;
        preauth_disconnect)  desc="认证前断开 (扫描器/快速探测)" ;;
        port_scan)           desc="未发送 SSH 标识 (端口扫描)" ;;
        pam_failure)         desc="PAM 认证失败" ;;
        max_attempts)        desc="超过最大认证尝试次数 (典型爆破特征)" ;;
        *)                   desc="" ;;
    esac
    printf "  ${C_MAGENTA}%-6s${C_RESET}  %-22s  %s\n" "$cnt" "$reason" "$desc"
done

# ---- 4. 成功登录记录 ----
if [[ $SHOW_SUCCESS -eq 1 ]]; then
    print_section "成功登录记录 (重点关注是否存在异常 IP / 时间)"
    if [[ "$SUCC_TOTAL" -eq 0 ]]; then
        echo "  (无成功登录记录)"
    else
        printf "  ${C_BOLD}%-6s  %-18s  %-37s  %-22s  %s${C_RESET}\n" \
            "次数" "来源 IP" "时间区间" "用户" "认证方式"
        # 以 IP 为主维度聚合: 同一 IP 的所有成功登录折叠为一行,
        # 列出涉及到的用户名集合 / 认证方式集合, 以及首末时间.
        # 这样活跃 IP 一眼可见, vscode-server/mosh 等高频重连不会刷屏.
        awk -F'\t' '
            {
                ip = $3
                if (!(ip in first)) first[ip] = $1
                last[ip] = $1
                cnt[ip]++
                if (!((ip SUBSEP $2) in seen_u)) {
                    seen_u[ip, $2] = 1
                    users[ip]   = (ip in users   ? users[ip]   "," $2 : $2)
                }
                if (!((ip SUBSEP $4) in seen_m)) {
                    seen_m[ip, $4] = 1
                    methods[ip] = (ip in methods ? methods[ip] "," $4 : $4)
                }
            }
            END {
                for (ip in cnt)
                    printf "%d\t%s\t%s\t%s\t%s\t%s\n", \
                        cnt[ip], ip, first[ip], last[ip], users[ip], methods[ip]
            }
        ' "$SUCC_TSV" | sort -t $'\t' -k1,1 -rn |
        while IFS=$'\t' read -r cnt ip first_ts last_ts users methods; do
            if [[ "$cnt" -gt 1 ]] && [[ "$first_ts" != "$last_ts" ]]; then
                ts_show="${first_ts} ~ ${last_ts}"
            else
                ts_show="$first_ts"
            fi
            printf "  ${C_GREEN}%-6s${C_RESET}  %-18s  %-37s  %-22s  %s\n" \
                "$cnt" "$ip" "$ts_show" "$users" "$methods"
        done
    fi
fi

# ---- 5. 原始日志 ----
if [[ $SHOW_RAW -eq 1 ]]; then
    print_section "原始命中日志 (前 200 行, 只显示 sshd 进程)"
    awk '
        # 同样限制只显示 sshd 行, 跟统计口径一致
        $0 !~ /sshd(-[a-z]+)?\[[0-9]+\]:/ { next }
        /Failed (password|publickey|none|keyboard-interactive)|Invalid user|Disconnected from (authenticating|invalid) user|Connection closed by (invalid|authenticating) user|Did not receive identification string|maximum authentication attempts exceeded|pam_unix\(sshd:.*authentication failure/ {print}
    ' "$RAW" |
    if [[ -n "$FILTER_IP" ]];   then grep -F "$FILTER_IP";   else cat; fi |
    if [[ -n "$FILTER_USER" ]]; then grep -wF "$FILTER_USER"; else cat; fi |
    head -n 200
fi

# ---- 6. 风险提示 ----
print_section "建议"
cat <<EOF
  - 若某个 IP 失败次数极高且尝试多个用户名, 基本可判定为爆破, 建议立即封禁:
      fail2ban-client set <jail> banip <ip>...
EOF

echo
log_info "完成 ✔"
