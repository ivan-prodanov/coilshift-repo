# csdk.sh: the car API helper a package vendors as lib/csdk.sh (SDK 1.0).
# bash 3.2 safe. Source it from a hook or service after the daemon's CSDK_*
# environment is set:  . "$CSDK_PACKAGE_DIR/lib/csdk.sh"

# csdk_api METHOD PATH [BODY]: one call to the car API. Scalar reads answer
# text/plain. A 4xx or 5xx answer prints its JSON error to stderr and fails.
csdk_api() {
    curl -sS --fail-with-body --unix-socket "$CSDK_API_SOCKET" \
        -H "Authorization: Bearer $CSDK_TOKEN" -H 'Content-Type: application/json' \
        -X "$1" "http://csdk/v1$2" ${3:+--data-binary "$3"}
}

# csdk_kv_put KEY VALUE: a raw value in the package's key-value store.
csdk_kv_put() {
    curl -sS --fail-with-body --unix-socket "$CSDK_API_SOCKET" \
        -H "Authorization: Bearer $CSDK_TOKEN" -H 'Content-Type: text/plain' \
        -X PUT "http://csdk/v1/kv/$1" --data-binary "$2"
}

# csdk_json_string TEXT: TEXT as a JSON string literal (quotes and backslashes
# escaped; the text must hold no control characters).
csdk_json_string() {
    local text=$1
    text=${text//\\/\\\\}
    text=${text//\"/\\\"}
    printf '"%s"' "$text"
}

# csdk_log LEVEL MESSAGE: one line in the daemon's log stream for this package.
csdk_log() {
    csdk_api POST /log "{\"level\":\"$1\",\"message\":$(csdk_json_string "$2")}" >/dev/null
}
