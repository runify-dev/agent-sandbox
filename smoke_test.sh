#!/usr/bin/env bash
# 沙箱冒烟测试：服务启动后在 sandbox 目录执行 ./smoke_test.sh .（读取 .env 里的 key）
set -u
cd "$1" || exit 1
KEY=$(grep '^SANDBOX_API_KEY=' .env | cut -d= -f2)
B=http://127.0.0.1:8090
H="X-API-Key: $KEY"
pass=0; fail=0
check() { # name expected actual
  if [[ "$3" == *"$2"* ]]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1"; echo "      expected: $2"; echo "      got: ${3:0:400}"; fail=$((fail+1)); fi
}
ex() { curl -s "$B/v1/exec" -H "$H" -H 'Content-Type: application/json' -d "$1"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

check "healthz" '"ok"' "$(curl -s $B/healthz)"
check "no key -> 401" "401" "$(code $B/v1/exec -d '{}')"
check "wrong key -> 401" "401" "$(code $B/v1/exec -H 'X-API-Key: wrongwrongwrongwrong' -d '{}')"
check "bad scope -> 400" "400" "$(code $B/v1/exec -H "$H" -d '{"scope":"../etc","language":"python","code":"print(1)"}')"
check "bad language -> 400" "400" "$(code $B/v1/exec -H "$H" -d '{"scope":"a","language":"ruby","code":"x"}')"

r=$(ex '{"scope":"a","language":"python","code":"import os,getpass,sys\nprint(os.getcwd(), os.getuid(), sys.version.split()[0])\nopen(\"hello.txt\",\"w\").write(\"hi from python\")"}')
check "python exec + cwd/uid" '/opt/workspace 1000 3.13' "$r"
check "python exit 0" '"exit_code":0' "$r"
c1=$(ex '{"scope":"a","language":"bash","code":"cat /etc/hostname"}')
c2=$(ex '{"scope":"a","language":"bash","code":"cat /etc/hostname"}')
check "container reused for same user" "$(echo "$c1" | head -c 60)" "$c2"
check "node exec" 'v22' "$(ex '{"scope":"a","language":"node","code":"console.log(process.version)"}')"
check "bash sees python file" 'hi from python' "$(ex '{"scope":"a","language":"bash","code":"cat hello.txt"}')"
check "ffmpeg present" 'ffmpeg version' "$(ex '{"scope":"a","language":"bash","code":"ffmpeg -version | head -1"}')"
check "cli tools present" 'git curl wget zip unzip jq' "$(ex '{"scope":"a","language":"bash","code":"for c in git curl wget zip unzip jq; do command -v $c >/dev/null && printf \"%s \" $c; done"}')"
check "git init + commit" 'GITOK' "$(ex '{"scope":"a","language":"bash","code":"rm -rf g && mkdir g && cd g && git init -q && git commit -q --allow-empty -m x && echo GIT\"OK\""}')"
check "zip roundtrip + jq" 'ZIPOK 2' "$(ex '{"scope":"a","language":"bash","code":"echo zz > z.txt && zip -q z.zip z.txt && rm z.txt && unzip -q z.zip && cat z.txt | grep -q zz && echo ZIP\"OK\" $(echo [1,2] | jq length)"}')"
check "db clients present" 'sqlite3 psql mysql redis-cli' "$(ex '{"scope":"a","language":"bash","code":"for c in sqlite3 psql mysql redis-cli; do command -v $c >/dev/null && printf \"%s \" $c; done"}')"
check "db python libs import" 'DBOK' "$(ex '{"scope":"a","language":"python","code":"import sqlalchemy, psycopg, pymysql, oracledb, pymssql, redis, pymongo, duckdb, clickhouse_connect\nprint(\"DB\" + \"OK\")"}')"
check "sqlalchemy + sqlite + duckdb" '42 42' "$(ex '{"scope":"a","language":"python","code":"import sqlalchemy as sa, duckdb\ne = sa.create_engine(\"sqlite:///t.db\")\nwith e.begin() as c:\n    c.execute(sa.text(\"create table if not exists t(v int)\")); c.execute(sa.text(\"delete from t\")); c.execute(sa.text(\"insert into t values (42)\"))\nprint(e.connect().execute(sa.text(\"select v from t\")).scalar(), duckdb.connect().execute(\"select 42\").fetchone()[0])"}')"
check "stderr + exit code" '"exit_code":3' "$(ex '{"scope":"a","language":"bash","code":"echo oops >&2; exit 3"}')"
check "env passthrough" 'BAR' "$(ex '{"scope":"a","language":"bash","code":"echo $FOO","env":{"FOO":"BAR"}}')"

# 上传 -> ffmpeg 处理 -> 下载
printf 'uploaded-content' > /tmp/sbx-up.txt
check "upload file" '"size":16' "$(curl -s -X PUT $B/v1/files/a/in/sub/up.txt -H "$H" --data-binary @/tmp/sbx-up.txt)"
check "container sees upload" 'uploaded-content' "$(ex '{"scope":"a","language":"bash","code":"cat in/sub/up.txt; ls -ln in/sub"}')"
check "uploaded file owned by 1000" ' 1000 1000 ' "$(ex '{"scope":"a","language":"bash","code":"ls -ln in/sub"}')"
r=$(ex '{"scope":"a","language":"bash","timeout":120,"code":"ffmpeg -loglevel error -y -f lavfi -i testsrc=duration=2:size=320x240:rate=25 -pix_fmt yuv420p in.mp4 && ffmpeg -loglevel error -y -i in.mp4 -vf scale=160:-2 out.mp4 && ls -l out.mp4"}')
check "ffmpeg transcode" 'out.mp4' "$r"
curl -s $B/v1/files/a/out.mp4 -H "$H" -o /tmp/sbx-out.mp4
check "download video (mp4 magic)" 'ftyp' "$(head -c 8 /tmp/sbx-out.mp4 | tail -c 4)"
check "range request -> 206" "206" "$(code $B/v1/files/a/out.mp4 -H "$H" -H 'Range: bytes=0-99')"
check "list dir" '"name":"out.mp4"' "$(curl -s $B/v1/files/a/ -H "$H")"
check "404 missing" "404" "$(code $B/v1/files/a/nope.txt -H "$H")"

# 对话隔离：同一个 scope 下，每个 session 有自己的容器和 /opt/workspace，/opt/user 共享
ex '{"scope":"a","session":"chatA","language":"bash","code":"echo slides-from-A > deck.pptx; echo note-for-all > /opt/user/shared.txt"}' >/dev/null
check "session sees own file" 'slides-from-A' "$(ex '{"scope":"a","session":"chatA","language":"bash","code":"cat deck.pptx"}')"
check "other session cannot see it" 'ISOLATED' "$(ex '{"scope":"a","session":"chatB","language":"bash","code":"ls /opt/workspace; [ -z \"$(find / -name deck.pptx 2>/dev/null)\" ] && echo ISOLATED"}')"
check "user space shared across sessions" 'note-for-all' "$(ex '{"scope":"a","session":"chatB","language":"bash","code":"cat /opt/user/shared.txt"}')"
check "no-session workspace is user space" 'note-for-all' "$(ex '{"scope":"a","language":"bash","code":"cat shared.txt"}')"
ha=$(ex '{"scope":"a","session":"chatA","language":"bash","code":"cat /etc/hostname"}' | head -c 40)
check "separate container per session" 'DIFFERENT' "$( [ "$ha" != "$(ex '{"scope":"a","session":"chatB","language":"bash","code":"cat /etc/hostname"}' | head -c 40)" ] && echo DIFFERENT)"
check "pip user base in user space" '/opt/user/.local' "$(ex '{"scope":"a","session":"chatA","language":"bash","code":"echo $PYTHONUSERBASE"}')"
check "file API reads session dir" 'slides-from-A' "$(curl -s "$B/v1/files/a/deck.pptx?session=chatA" -H "$H")"
check "file API without session misses it" "404" "$(code "$B/v1/files/a/deck.pptx" -H "$H")"
check "upload into session" '"size":3' "$(curl -s -X PUT "$B/v1/files/a/in.txt?session=chatB" -H "$H" --data-binary 'abc')"
check "session sees uploaded file" 'abc' "$(ex '{"scope":"a","session":"chatB","language":"bash","code":"cat in.txt"}')"
check "invalid session -> 400" "400" "$(code $B/v1/exec -H "$H" -d '{"scope":"a","session":"../x","language":"bash","code":"id"}')"
check "invalid session in file API -> 400" "400" "$(code "$B/v1/files/a/x?session=..%2Fx" -H "$H")"
check "purge session" '"removed"' "$(curl -s -X DELETE "$B/v1/scopes/a/sessions/chatA?purge=true" -H "$H")"
check "purged session data gone" "404" "$(code "$B/v1/files/a/deck.pptx?session=chatA" -H "$H")"
check "other session untouched" 'abc' "$(curl -s "$B/v1/files/a/in.txt?session=chatB" -H "$H")"

# 安全
check "scope b isolated from a" 'No such file' "$(ex '{"scope":"b","language":"bash","code":"cat /opt/workspace/hello.txt 2>&1; ls /opt"}')"
ex '{"scope":"a","language":"bash","code":"ln -sf /etc/passwd leak; ln -sfn / rootlink; ln -sfn .. up; mkfifo fifo 2>/dev/null; true"}' >/dev/null
check "symlink to /etc/passwd blocked" "400" "$(code $B/v1/files/a/leak -H "$H")"
check "symlink to / blocked" "400" "$(code $B/v1/files/a/rootlink/etc/passwd -H "$H")"
check "symlink .. blocked" "400" "$(code $B/v1/files/a/up/a/workspace -H "$H")"
check "upload through symlink blocked" "400" "$(code -X PUT $B/v1/files/a/rootlink/tmp/pwn -H "$H" --data-binary x)"
check "url ../ normalized" "404" "$(code -L --path-as-is $B/v1/files/a/../../etc/passwd -H "$H")"
check "fifo not readable, no hang" "400" "$(code -m 5 $B/v1/files/a/fifo -H "$H")"
check "network disabled" 'NETFAIL' "$(ex '{"scope":"a","language":"python","timeout":10,"code":"import urllib.request\ntry:\n  urllib.request.urlopen(\"http://1.1.1.1\", timeout=3)\n  print(\"NETOK\")\nexcept Exception:\n  print(\"NETFAIL\")"}')"
check "rootfs read-only" 'Read-only' "$(ex '{"scope":"a","language":"bash","code":"touch /usr/x 2>&1"}')"
check "no root" 'uid=1000' "$(ex '{"scope":"a","language":"bash","code":"id"}')"
r=$(ex '{"scope":"a","language":"bash","timeout":2,"code":"sleep 100 & sleep 100"}')
check "timeout kills process group" '"timed_out":true' "$r"
check "no leftover sleep" 'CLEAN' "$(ex '{"scope":"a","language":"bash","code":"pgrep -x sleep | grep -v \"^1$\" | wc -l | grep -q \"^1$\" && echo CLEAN || ps -ef"}')"

# 预装包
check "preinstalled packages" 'IMPORTOK' "$(ex '{"scope":"a","language":"python","timeout":60,"code":"import docx, pptx, pypdf, pdfplumber, reportlab, openpyxl, cv2, sklearn, scipy, bs4, yaml, httpx\nprint(\"IMPORTOK\")"}')"
check "office python libs" 'OFFICEOK' "$(ex '{"scope":"a","language":"python","timeout":60,"code":"import docxtpl, docx2python, mammoth, xlsxwriter, xlwt, pyxlsb, odf, pikepdf, pdf2image, fpdf, weasyprint, pypandoc, pytesseract, html2text\nprint(\"OFFICE\" + \"OK\")"}')"
r=$(ex '{"scope":"a","session":"office","language":"python","timeout":180,"code":"import subprocess, docx, pypandoc, pytesseract, weasyprint\nfrom pdf2image import convert_from_path\nd = docx.Document(); d.add_paragraph(\"你好 MaxKB 沙箱\"); d.save(\"t.docx\")\nsubprocess.run([\"soffice\", \"--headless\", \"--convert-to\", \"pdf\", \"t.docx\"], check=True, capture_output=True, timeout=150)\nprint(\"SOFFICE\", len(open(\"t.pdf\", \"rb\").read()) > 1000)\nimg = convert_from_path(\"t.pdf\", dpi=150)[0]; img.save(\"t.png\")\nprint(\"PDF2IMAGE\", img.size[0] > 500)\nprint(\"OCR\", \"MaxKB\" in pytesseract.image_to_string(img, lang=\"chi_sim+eng\"))\npypandoc.convert_text(\"# 标题\", \"docx\", format=\"md\", outputfile=\"t2.docx\")\nprint(\"PANDOC\", len(open(\"t2.docx\", \"rb\").read()) > 1000)\nweasyprint.HTML(string=\"<h1>中文</h1>\").write_pdf(\"t3.pdf\")\nprint(\"WEASY\", len(open(\"t3.pdf\", \"rb\").read()) > 1000)"}')
for tool in "SOFFICE True" "PDF2IMAGE True" "OCR True" "PANDOC True" "WEASY True"; do check "office: ${tool% *}" "$tool" "$r"; done
curl -s -X DELETE "$B/v1/scopes/a/sessions/office?purge=true" -H "$H" >/dev/null
check "matplotlib chinese font" 'WenQuanYi' "$(ex '{"scope":"a","language":"python","code":"import matplotlib\nprint(matplotlib.rcParams[\"font.sans-serif\"][0])"}')"

# 白名单代理（SANDBOX_NETWORK=proxy 时）
if docker compose logs --no-log-prefix sandbox-server 2>/dev/null | grep 'sandbox server listening' | tail -n 1 | grep -q '"network":"proxy"'; then
  check "pip install from allowlisted pypi" 'PIPOK' "$(ex '{"scope":"a","language":"bash","timeout":120,"code":"pip install --user -q cowsay 2>&1 | tail -3; python3 -c \"import cowsay; print(\\\"PIP\\\" + \\\"OK\\\")\""}')"
  check "non-allowlisted host denied (403)" '403 Forbidden' "$(ex '{"scope":"a","language":"python","timeout":30,"code":"import requests\ntry:\n  requests.get(\"https://example.com\", timeout=10)\n  print(\"REACHED\")\nexcept Exception as e:\n  print(e)"}')"
  check "bypassing proxy fails" 'DIRECTFAIL' "$(ex '{"scope":"a","language":"python","timeout":30,"code":"import requests\ns = requests.Session(); s.trust_env = False\ntry:\n  s.get(\"https://pypi.org\", timeout=5)\n  print(\"REACHED\")\nexcept Exception:\n  print(\"DIRECTFAIL\")"}')"
  check "sandbox cannot call management API" 'STATUS 403' "$(ex '{"scope":"a","language":"python","timeout":30,"code":"import requests\ns = requests.Session(); s.trust_env = False\nprint(\"STATUS\", s.get(\"http://sandbox-egress-proxy:8090/healthz\", timeout=5).status_code)"}')"
  check "proxy refuses internal targets" 'not in allowlist' "$(ex '{"scope":"a","language":"python","timeout":30,"code":"import requests\ntry:\n  requests.get(\"http://sandbox-egress-proxy:8090/healthz\", timeout=5)\n  print(\"REACHED\")\nexcept Exception as e:\n  print(e)\nr = requests.get(\"http://169.254.169.254/latest/meta-data/\", timeout=5)\nprint(r.status_code, r.text)"}')"
fi

check "delete file" '"deleted"' "$(curl -s -X DELETE $B/v1/files/a/in -H "$H")"
check "delete root refused" "400" "$(code -X DELETE $B/v1/files/a/ -H "$H")"
check "remove scope" '"removed"' "$(curl -s -X DELETE $B/v1/scopes/b -H "$H")"
check "purge whole scope" '"removed"' "$(curl -s -X DELETE "$B/v1/scopes/purgeme?purge=true" -H "$H")"
echo "== $pass passed, $fail failed"
