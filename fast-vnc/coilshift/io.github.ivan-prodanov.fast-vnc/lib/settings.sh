# settings.sh: Fast VNC's two settings, shared by the service and the apply hook. Bash 3.2 safe.
# Needs CSDK_SETTINGS.

# fv_setting ID: the setting's value without line breaks; empty when unset.
fv_setting() { [ -f "$CSDK_SETTINGS/$1" ] && tr -d '\r\n' < "$CSDK_SETTINGS/$1"; }

# fv_settings_sum: a checksum of the settings the service starts with, so the apply hook can tell
# whether they changed since (the password itself is not stored a second time).
fv_settings_sum() { printf '%s\n%s\n' "$(fv_setting size)" "$(fv_setting password)" | cksum; }
