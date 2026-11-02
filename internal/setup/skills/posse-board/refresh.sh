#!/bin/sh
# Usage: sh refresh.sh <board-dir>, run from the Project's repository.
# Copies the board template when it changed and rewrites the two data files.
set -eu

board=$1
skill=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$board"
cmp -s "$skill/board.html" "$board/index.html" || cp "$skill/board.html" "$board/index.html"

# Each file is one posseBoardData call around raw JSON, written by rename so
# the page never reads half a file.
write() {
	printf 'posseBoardData("%s", %s);\n' "$1" "$2" >"$board/$1.js.tmp"
	mv -f "$board/$1.js.tmp" "$board/$1.js"
}

if posse_data=$(posse --full --json); then
	:
else
	echo "posse-board: posse --full --json failed; board data was not updated" >&2
	exit 1
fi
write posse "$posse_data"
if issues=$(gh issue list --label ready-for-agent --state all --limit 200 \
	--json number,title,state,body,url,createdAt,closedAt,labels); then
	write issues "$issues"
else
	echo "posse-board: gh issue list failed; the board shows no issues" >&2
	write issues "[]"
fi
echo "$board/index.html"
