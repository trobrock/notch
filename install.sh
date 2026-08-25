#!/bin/sh
set -eu

repository=trobrock/notch
releases_url="https://github.com/$repository/releases"
install_dir=${NOTCH_INSTALL_DIR:-${HOME:+$HOME/.local/bin}}
version=${NOTCH_VERSION:-latest}

usage() {
  cat <<'EOF'
Install a verified Notch release for Linux or macOS.

Usage: install.sh [--version VERSION] [--install-dir DIRECTORY]

Options:
  --version VERSION        Release to install (for example, v0.2.0). The
                           default is the latest stable release.
  --install-dir DIRECTORY  Installation directory. The default is
                           $NOTCH_INSTALL_DIR or $HOME/.local/bin.
  -h, --help               Show this help.

Environment:
  NOTCH_VERSION            Same as --version.
  NOTCH_INSTALL_DIR        Same as --install-dir.
EOF
}

fail() {
  echo "notch installer: $*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case $1 in
    --version)
      [ "$#" -ge 2 ] || fail "--version requires a value"
      version=$2
      shift 2
      ;;
    --version=*)
      version=${1#*=}
      shift
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || fail "--install-dir requires a value"
      install_dir=$2
      shift 2
      ;;
    --install-dir=*)
      install_dir=${1#*=}
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[ -n "$install_dir" ] || fail "HOME is not set; pass --install-dir"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case $(uname -m) in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

curl_download() {
  download_url=$1
  destination=$2
  maximum_size=$3
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --fail --silent --show-error --location --retry 3 \
    --max-filesize "$maximum_size" --output "$destination" "$download_url"
  downloaded_size=$(wc -c <"$destination" | tr -d '[:space:]')
  [ "$downloaded_size" -le "$maximum_size" ] || fail "download from $download_url is too large"
}

if [ "$version" = latest ]; then
  effective_url=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --fail --silent --show-error --location --retry 3 \
    --output /dev/null --write-out '%{url_effective}' "$releases_url/latest")
  version=${effective_url##*/}
else
  case $version in
    v*) ;;
    *) version=v$version ;;
  esac
fi

if ! printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'; then
  fail "invalid release version: $version"
fi

archive="notch_${version#v}_${os}_${arch}.tar.gz"
temp_root=${TMPDIR:-/tmp}
[ -d "$temp_root" ] || fail "temporary directory does not exist: $temp_root"
temp_root=$(cd "$temp_root" && pwd -P) || fail "could not access temporary directory: $temp_root"
work_dir=$(mktemp -d "$temp_root/notch-install.XXXXXX") || fail "could not create a temporary directory"
install_tmp=
cleanup() {
  rm -rf "$work_dir"
  if [ -n "$install_tmp" ]; then
    rm -f "$install_tmp"
  fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

base_url="$releases_url/download/$version"
echo "Downloading Notch $version for $os/$arch..."
curl_download "$base_url/checksums.txt" "$work_dir/checksums.txt" 1048576
curl_download "$base_url/$archive" "$work_dir/$archive" 268435456

expected=$(awk -v name="$archive" '
  NF == 2 && ($2 == name || $2 == "*" name) { count++; digest = $1 }
  END { if (count != 1) exit 1; print digest }
' "$work_dir/checksums.txt") || fail "checksums.txt has no unique entry for $archive"

case $expected in
  *[!0-9A-Fa-f]*|'') fail "release checksum for $archive is malformed" ;;
esac
[ "${#expected}" -eq 64 ] || fail "release checksum for $archive is malformed"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$work_dir/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$work_dir/$archive" | awk '{ print $1 }')
elif command -v openssl >/dev/null 2>&1; then
  actual=$(openssl dgst -sha256 "$work_dir/$archive" | awk '{ print $NF }')
else
  fail "sha256sum, shasum, or openssl is required to verify the download"
fi

expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
[ "$actual" = "$expected" ] || fail "checksum verification failed for $archive"

entries=$(tar -tzf "$work_dir/$archive") || fail "could not inspect $archive"
[ "$entries" = notch ] || fail "release archive must contain only a root-level notch executable"
tar -xzf "$work_dir/$archive" -C "$work_dir" notch || fail "could not extract $archive"
[ -f "$work_dir/notch" ] && [ ! -L "$work_dir/notch" ] || fail "release archive does not contain a regular notch executable"

mkdir -p "$install_dir" || fail "could not create $install_dir"
if [ -e "$install_dir/notch" ] || [ -L "$install_dir/notch" ]; then
  [ -f "$install_dir/notch" ] && [ ! -L "$install_dir/notch" ] || fail "$install_dir/notch is not a regular file"
fi
install_tmp=$(mktemp "$install_dir/.notch-install.XXXXXX") || fail "cannot write to $install_dir"
cat "$work_dir/notch" >"$install_tmp" || fail "could not copy notch to $install_dir"
chmod 755 "$install_tmp" || fail "could not make notch executable"
installed_version=$("$install_tmp" --version) || fail "the downloaded binary could not run"
[ "$installed_version" = "notch $version" ] || fail "downloaded binary reported unexpected version: $installed_version"
mv -f "$install_tmp" "$install_dir/notch" || fail "could not install notch in $install_dir"
install_tmp=

echo "Installed $installed_version at $install_dir/notch"
case :${PATH:-}: in
  *:"$install_dir":*) ;;
  *) echo "Add $install_dir to PATH to run notch." ;;
esac
