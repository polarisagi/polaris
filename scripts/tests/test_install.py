#!/usr/bin/env python3
# 只在临时 HOME 中执行真实安装脚本，替代下载和系统命令以避免改动宿主环境。
import hashlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def executable(path, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text('#!/bin/bash\n' + content)
    path.chmod(0o755)


def archive(path, entries):
    with tarfile.open(path, 'w:gz') as out:
        for name, data in entries.items():
            data = data.encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o755
            out.addfile(info, io.BytesIO(data))


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='polaris-install-test-')
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.home = self.base / 'fresh home'
        self.home.mkdir()
        self.bin = self.base / 'tools'
        self.bin.mkdir()
        for cmd in ('awk', 'basename', 'bash', 'cat', 'chmod', 'cp', 'dirname', 'env', 'find',
                    'grep', 'gzip', 'head', 'install', 'ln', 'mkdir', 'mktemp',
                    'openssl', 'rm', 'sed', 'sh', 'tar', 'tr'):
            target = shutil.which(cmd)
            self.assertIsNotNone(target, cmd)
            (self.bin / cmd).symlink_to(target)
        executable(self.bin / 'uname', 'if [ "$1" = -s ]; then echo "${MOCK_OS:-Linux}"; else echo arm64; fi\n')
        executable(self.bin / 'id', 'echo 0\n')
        executable(self.bin / 'sleep', 'exit 0\n')
        executable(self.bin / 'systemctl', 'exit 0\n')
        executable(self.bin / 'xcode-select', '[ "$1" = --install ]\n')
        executable(self.bin / 'git', 'exit 0\n')
        executable(self.bin / 'bwrap', 'exit 0\n')
        executable(self.bin / 'apt-get', '''printf '%s\n' "$*" >> "$MOCK_BASE/packages"
for cmd in git bwrap; do
    printf '#!/bin/bash\\nexit 0\\n' > "$MOCK_TOOLS/$cmd"
    chmod +x "$MOCK_TOOLS/$cmd"
done
''')
        uv = '''#!/bin/bash
if [ "$1" = python ] && [ "$2" = install ]; then
    [ "${MOCK_PY_FAIL:-0}" != 1 ] || exit 1
    mkdir -p "$HOME/managed-python"
    printf '#!/bin/bash\\nexit 0\\n' > "$HOME/managed-python/python3"
    chmod +x "$HOME/managed-python/python3"
elif [ "$1" = python ] && [ "$2" = find ]; then
    [ -f "$HOME/managed-python/python3" ] || exit 1
    echo "$HOME/managed-python/python3"
fi
'''
        (self.base / 'uv').write_text(uv)
        (self.base / 'uv-install.sh').write_text('''#!/bin/bash
[ "${MOCK_UV_FAIL:-0}" != 1 ] || exit 1
mkdir -p "$UV_INSTALL_DIR"
cp "$MOCK_BASE/uv" "$UV_INSTALL_DIR/uv"
cp "$MOCK_BASE/uv" "$UV_INSTALL_DIR/uvx"
chmod +x "$UV_INSTALL_DIR/uv" "$UV_INSTALL_DIR/uvx"
''')
        archive(self.base / 'node.tar.gz', {
            'node-v24.1.0/bin/' + cmd: '#!/bin/bash\n[ "${MOCK_NODE_FAIL:-0}" != 1 ]\n'
            for cmd in ('node', 'npm', 'npx')
        })
        sha = hashlib.sha256((self.base / 'node.tar.gz').read_bytes()).hexdigest()
        (self.base / 'node-sha').write_text(sha + '  node-v24.1.0-linux-arm64.tar.gz\n' +
                                            sha + '  node-v24.1.0-darwin-arm64.tar.gz\n')
        archive(self.base / 'polaris.tar.gz', {'polaris/polaris': '''#!/bin/bash
case "$1" in
    version) [ "${MOCK_PRECHECK_FAIL:-0}" != 1 ] ;;
    service)
        if [ "$2" = install ]; then
            printf '%s' "$PATH" > "$HOME/service-path"
            [ "${MOCK_SERVICE_FAIL:-0}" != 1 ]
        elif [ "${MOCK_NOT_READY:-0}" = 1 ]; then
            echo '{"running":false}'
        else
            echo '{"running":true,"base_url":"http://127.0.0.1:29999"}'
        fi ;;
esac
'''})
        sha = hashlib.sha256((self.base / 'polaris.tar.gz').read_bytes()).hexdigest()
        (self.base / 'polaris-sha').write_text(sha + '  polaris.tar.gz\n')
        executable(self.bin / 'curl', '''out=""; url=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --max-time|-m|-C) shift 2 ;;
        https://*) url="$1"; shift ;;
        *) shift ;;
    esac
done
case "$url" in
    *ipinfo.io/country) echo US; exit 0 ;;
    *astral.sh/uv/install.sh) src="$MOCK_BASE/uv-install.sh" ;;
    *SHASUMS256.txt) src="$MOCK_BASE/node-sha" ;;
    *node-v*.tar.gz) src="$MOCK_BASE/node.tar.gz" ;;
    *polaris-*.tar.gz.sha256)
        [ "${MOCK_SHA_FAIL:-0}" != 1 ] || exit 22
        src="$MOCK_BASE/polaris-sha" ;;
    *polaris-*.tar.gz) src="$MOCK_BASE/polaris.tar.gz" ;;
    *) exit 22 ;;
esac
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
''')
        self.env = {k: v for k, v in os.environ.items()
                    if k not in ('LANG', 'LC_ALL', 'LANGUAGE', 'NVM_DIR')}
        self.env.update(HOME=str(self.home), PATH=str(self.bin), SHELL='/bin/bash',
                        TMPDIR=str(self.base), MOCK_BASE=str(self.base), MOCK_TOOLS=str(self.bin))

    def run_install(self, **env):
        result = subprocess.run(['/bin/bash', str(ROOT / 'scripts/install.sh')],
                                env=dict(self.env, **env), capture_output=True, text=True, timeout=15)
        self.assertFalse(list(self.base.glob('polaris-install.*')), result.stdout + result.stderr)
        return result

    def test_fresh_machine_and_idempotent_path(self):
        for _ in range(2):
            result = self.run_install()
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.home / '.local/bin/python3').is_symlink())
        self.assertTrue((self.home / '.local/bin/node').is_symlink())
        self.assertIn(str(self.home / '.local/bin'), (self.home / 'service-path').read_text())
        for profile in ('.bashrc', '.profile'):
            self.assertEqual((self.home / profile).read_text().count('# >>> polaris path >>>'), 1)

    def test_missing_linux_git_and_bubblewrap(self):
        (self.bin / 'git').unlink()
        (self.bin / 'bwrap').unlink()
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('bubblewrap', (self.base / 'packages').read_text())

    def test_dependency_and_service_failures_report_incomplete(self):
        for variable in ('MOCK_UV_FAIL', 'MOCK_PY_FAIL', 'MOCK_NODE_FAIL',
                         'MOCK_SERVICE_FAIL', 'MOCK_NOT_READY'):
            with self.subTest(variable=variable):
                # 每个失败场景独立 HOME，避免前一轮安装掩盖后续缺失。
                self.env['HOME'] = str(self.base / variable)
                result = self.run_install(**{variable: '1'})
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn('🎉', result.stdout)
                self.assertTrue((Path(self.env['HOME']) / '.polaris/bin/polaris').exists())

    def test_invalid_download_preserves_old_install(self):
        old = self.home / '.polaris/bin/polaris'
        old.parent.mkdir(parents=True)
        old.write_text('old-install')
        (self.base / 'polaris-sha').write_text('0' * 64 + '  archive\n')
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(old.read_text(), 'old-install')
        self.assertFalse((self.home / 'service-path').exists())

    def test_preflight_failure_preserves_old_install(self):
        old = self.home / '.polaris/bin/polaris'
        old.parent.mkdir(parents=True)
        old.write_text('old-install')
        result = self.run_install(MOCK_PRECHECK_FAIL='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(old.read_text(), 'old-install')

    def test_macos_without_clt_continues_with_warning(self):
        (self.bin / 'git').unlink()
        result = self.run_install(MOCK_OS='Darwin')
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((self.home / '.polaris/bin/polaris').exists())
        self.assertIn('Command Line', result.stdout)


if __name__ == '__main__':
    unittest.main()
