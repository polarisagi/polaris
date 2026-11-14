#!/usr/bin/env python3
# 只在临时 HOME 中执行真实安装脚本，替代下载和系统命令以避免改动宿主环境。
import hashlib
import io
import os
from pathlib import Path
import re
import base64
import shutil
import subprocess
import tarfile
import tempfile
import unittest
import zipfile

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


def wheel(path, entries):
    with zipfile.ZipFile(path, 'w', compression=zipfile.ZIP_DEFLATED) as out:
        for name, data in entries.items():
            out.writestr(name, data)


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='polaris-install-test-')
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)
        self.home = self.base / 'fresh home'
        self.home.mkdir()
        self.bin = self.base / 'tools'
        self.bin.mkdir()
        self.key = self.base / 'test-key.pem'
        self.pub = self.base / 'test-public.pem'
        subprocess.run(['openssl', 'genpkey', '-algorithm', 'EC', '-pkeyopt', 'ec_paramgen_curve:P-256',
                        '-out', str(self.key)], check=True, capture_output=True)
        subprocess.run(['openssl', 'pkey', '-in', str(self.key), '-pubout', '-out', str(self.pub)],
                       check=True, capture_output=True)
        for cmd in ('awk', 'basename', 'bash', 'cat', 'chmod', 'cp', 'dirname', 'env', 'find',
                    'grep', 'gzip', 'head', 'install', 'ln', 'mkdir', 'mktemp',
                    'mv', 'openssl', 'rm', 'sed', 'sh', 'tar', 'tr', 'unzip'):
            target = shutil.which(cmd)
            self.assertIsNotNone(target, cmd)
            (self.bin / cmd).symlink_to(target)
        executable(self.bin / 'uname', 'if [ "$1" = -s ]; then echo "${MOCK_OS:-Linux}"; else echo arm64; fi\n')
        executable(self.bin / 'id', 'echo 0\n')
        executable(self.bin / 'sleep', 'exit 0\n')
        executable(self.bin / 'systemctl', 'exit 0\n')
        executable(self.bin / 'xcode-select', '[ "$1" = --install ]\n')
        executable(self.bin / 'brew', 'exit 1\n')
        executable(self.bin / 'port', 'exit 1\n')
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
            'node-v24.22.3/bin/node': '''#!/bin/bash
[ "${MOCK_NODE_FAIL:-0}" != 1 ] || exit 1
[ "$1" = --version ] && { echo v24.22.3; exit 0; }
exit 0
''',
            'node-v24.22.3/bin/npm': '''#!/bin/bash
if [ "$1" = config ] && [ "$2" = get ]; then echo "${MOCK_NPM_REGISTRY:-https://registry.npmjs.org/}"; exit 0; fi
if [ "$1" = config ] && [ "$2" = set ]; then printf '%s\n' "$*" >> "$MOCK_BASE/npm-config"; exit 0; fi
if [ "$1" = --version ]; then echo 10.9.0; exit 0; fi
[ "${MOCK_NODE_FAIL:-0}" != 1 ]
''',
            'node-v24.22.3/bin/npx': '#!/bin/bash\n[ "$1" = --version ] && { echo 10.9.0; exit 0; }\n[ "${MOCK_NODE_FAIL:-0}" != 1 ]\n'
        })
        sha = hashlib.sha256((self.base / 'node.tar.gz').read_bytes()).hexdigest()
        self.node_sha = sha
        uv_bundle = '''#!/bin/bash
        if [ "$1" = --version ]; then echo "uv 0.13.7 (fixture aarch64-unknown-linux-gnu)"; exit 0; fi
if [ "$1" = python ] && [ "$2" = install ]; then
    printf '%s\n' "${UV_PYTHON_INSTALL_MIRROR:-unset}" >> "$MOCK_BASE/python-mirrors"
    [ "${MOCK_PY_FAIL:-0}" != 1 ] || exit 1
    mkdir -p "$HOME/managed-python"
    printf '#!/bin/bash\\nexit 0\\n' > "$HOME/managed-python/python3"
    chmod +x "$HOME/managed-python/python3"
elif [ "$1" = python ] && [ "$2" = find ]; then
    [ -f "$HOME/managed-python/python3" ] || exit 1
    echo "$HOME/managed-python/python3"
fi
'''
        wheel(self.base / 'uv.whl', {
            'uv-0.13.7.data/scripts/uv': uv_bundle,
            'uv-0.13.7.data/scripts/uvx': uv_bundle,
            'uv-0.13.7.dist-info/licenses/LICENSE-MIT': 'MIT license fixture\n',
        })
        self.uv_sha = hashlib.sha256((self.base / 'uv.whl').read_bytes()).hexdigest()
        wheel(self.base / 'uv-missing-uvx.whl', {'uv-0.13.7.data/scripts/uv': uv_bundle})
        self.uv_missing_sha = hashlib.sha256((self.base / 'uv-missing-uvx.whl').read_bytes()).hexdigest()
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
        subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.key), '-out',
                        str(self.base / 'polaris-sig.der'), str(self.base / 'polaris-sha')], check=True)
        subprocess.run(['openssl', 'base64', '-A', '-in', str(self.base / 'polaris-sig.der'), '-out',
                        str(self.base / 'polaris-sig')], check=True)
        executable(self.bin / 'curl', '''out=""; url=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        --max-time|-m|-C) shift 2 ;;
        http://*|https://*) url="$1"; shift ;;
        *) shift ;;
    esac
done
printf '%s\n' "$url" >> "$MOCK_BASE/urls"
case "$url" in
    *uv-0.13.7-*.whl)
        [ "${MOCK_UV_ASSET_FAIL:-0}" != 1 ] || exit 22
        if [ "${MOCK_MIRROR_ONLY:-0}" = 1 ] && [[ "$url" != *mirrors.aliyun.com* ]] && [[ "$url" != *pythonhosted.org* ]]; then exit 22; fi
        if [ "${MOCK_UVX_MISSING:-0}" = 1 ]; then src="$MOCK_BASE/uv-missing-uvx.whl"; else src="$MOCK_BASE/uv.whl"; fi ;;
    *127.0.0.1:29999/healthz|*127.0.0.1:29999/) printf 200; exit 0 ;;
    *SHASUMS256.txt) exit 22 ;;
    *node-v24.22.3-*.tar.gz)
        [ "${MOCK_NODE_ASSET_FAIL:-0}" != 1 ] || exit 22
        if [ "${MOCK_MIRROR_ONLY:-0}" = 1 ] && [[ "$url" != "${POLARIS_DOWNLOAD_BASE:-https://npmmirror.com/mirrors/node}/"* ]] && [[ "$url" != *npmmirror.com* ]]; then exit 22; fi
        [ "${MOCK_BAD_NODE_SHA:-0}" != 1 ] || { cp "$MOCK_BASE/node.tar.gz" "$out"; printf bad >> "$out"; exit 0; }
        src="$MOCK_BASE/node.tar.gz" ;;
    *polaris-*.tar.gz.sha256.sig) src="$MOCK_BASE/polaris-sig" ;;
    *polaris-*.tar.gz.sha256)
        [ "${MOCK_SHA_FAIL:-0}" != 1 ] || exit 22
        src="$MOCK_BASE/polaris-sha" ;;
    *polaris-*.tar.gz) src="$MOCK_BASE/polaris.tar.gz" ;;
    *) exit 22 ;;
esac
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
''')
        self.env = {k: v for k, v in os.environ.items()
                    if k not in ('LANG', 'LC_ALL', 'LANGUAGE', 'NVM_DIR', 'XDG_CONFIG_HOME',
                                 'PIP_CONFIG_FILE', 'UV_CONFIG_FILE', 'UV_DEFAULT_INDEX', 'UV_INDEX_URL',
                                 'UV_PYTHON_INSTALL_MIRROR', 'PIP_INDEX_URL', 'NPM_CONFIG_REGISTRY',
                                 'NPM_CONFIG_USERCONFIG', 'POLARIS_CONFIGURE_MIRRORS')}
        self.env.update(HOME=str(self.home), PATH=str(self.bin), SHELL='/bin/bash',
                        TMPDIR=str(self.base), MOCK_BASE=str(self.base), MOCK_TOOLS=str(self.bin),
                        XDG_CONFIG_HOME=str(self.base / 'xdg-config'))

    def run_install(self, **env):
        installer = self.base / 'install.sh'
        source = (ROOT / 'scripts/install.sh').read_text()
        source = source.replace('261af2af250dfd7e9f25cf151d1b2713a0adf8f10a9ad116c4947f68627affe8', self.node_sha)
        source = source.replace('a90f5d95716ab83091f0f73a8e7ec64e557abf2a28537fcd719b2ac88e6f725a', self.uv_sha)
        if env.get('MOCK_UVX_MISSING') == '1':
            source = source.replace(self.uv_sha, self.uv_missing_sha)
        der = subprocess.run(['openssl', 'pkey', '-pubin', '-in', str(self.pub), '-outform', 'DER'],
                             check=True, capture_output=True).stdout
        key = base64.b64encode(der).decode()
        source, count = re.subn(r'(?ms)(# RELEASE_PUBLIC_KEYS_BEGIN\nRELEASE_PUBLIC_KEYS=\(\n).*?(\n\)\n# RELEASE_PUBLIC_KEYS_END)',
                                lambda match: match.group(1) + "    '" + key + "'" + match.group(2), source)
        self.assertEqual(count, 1)
        installer.write_text(source)
        run_env = dict(self.env, **env)
        result = subprocess.run(['/bin/bash', str(installer)],
                                env=run_env, capture_output=True, text=True, timeout=20)
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

    def test_existing_dependencies_are_reused_without_downloads(self):
        second = self.base / 'second tools'
        second.mkdir()
        executable(self.bin / 'python3', 'exit 1\n')
        executable(second / 'python3', 'exit 0\n')
        executable(self.bin / 'uv', 'exit 1\n')
        executable(self.bin / 'uvx', 'exit 1\n')
        executable(second / 'uv', '[ "$1" = --version ] && exit 0\nexit 1\n')
        executable(second / 'uvx', '[ "$1" = --version ] && exit 0\nexit 1\n')
        executable(self.bin / 'node', 'exit 1\n')
        executable(self.bin / 'npm', 'exit 1\n')
        executable(self.bin / 'npx', 'exit 1\n')
        executable(second / 'node', 'exit 0\n')
        executable(second / 'npm', 'exit 0\n')
        executable(second / 'npx', 'exit 0\n')
        self.env['PATH'] = f"{self.bin}:{second}"
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        urls = (self.base / 'urls').read_text()
        self.assertNotIn('.whl', urls)
        self.assertNotIn('node-v24.22.3-', urls)
        self.assertNotIn('uv ', (self.base / 'packages').read_text() if (self.base / 'packages').exists() else '')

    def test_opt_in_mirrors_are_idempotent_and_default_is_read_only(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        config = self.base / 'xdg-config/uv/uv.toml'
        self.assertFalse(config.exists())
        self.assertFalse((self.base / 'npm-config').exists())
        result = self.run_install(POLARIS_CONFIGURE_MIRRORS='1')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        contents = config.read_text()
        self.assertLess(contents.index('python-install-mirror'), contents.index('[[index]]'))
        self.assertIn('registry.npmmirror.com/-/binary/python-build-standalone', contents)
        self.assertIn('mirrors.aliyun.com/pypi/simple/', contents)
        (self.base / 'urls').write_text('')
        result = self.run_install(POLARIS_CONFIGURE_MIRRORS='1')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(config.read_text(), contents)

    def test_missing_linux_git_and_bubblewrap(self):
        (self.bin / 'git').unlink()
        (self.bin / 'bwrap').unlink()
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('bubblewrap', (self.base / 'packages').read_text())

    def test_dependency_and_service_failures_report_incomplete(self):
        for variable in ('MOCK_UV_ASSET_FAIL', 'MOCK_PY_FAIL', 'MOCK_NODE_FAIL',
                         'MOCK_SERVICE_FAIL', 'MOCK_NOT_READY'):
            with self.subTest(variable=variable):
                # 每个失败场景独立 HOME，避免前一轮安装掩盖后续缺失。
                home = self.base / variable
                home.mkdir()
                self.env['HOME'] = str(home)
                old = home / '.polaris/bin/polaris'
                old.parent.mkdir(parents=True)
                old.write_text('old-install')
                result = self.run_install(**{variable: '1'})
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn('🎉', result.stdout)
                if variable in ('MOCK_UV_ASSET_FAIL', 'MOCK_PY_FAIL', 'MOCK_NODE_FAIL'):
                    self.assertEqual(old.read_text(), 'old-install')

    def test_invalid_download_preserves_old_install(self):
        old = self.home / '.polaris/bin/polaris'
        old.parent.mkdir(parents=True)
        old.write_text('old-install')
        (self.base / 'polaris-sha').write_text('0' * 64 + '  archive\n')
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(old.read_text(), 'old-install')
        self.assertFalse((self.home / 'service-path').exists())

    def test_missing_or_tampered_signature_preserves_old_install(self):
        for mode in ('missing', 'tampered'):
            with self.subTest(mode=mode):
                old = self.home / '.polaris/bin/polaris'
                old.parent.mkdir(parents=True, exist_ok=True)
                old.write_text('old-install')
                if mode == 'missing':
                    (self.base / 'polaris-sig').unlink()
                else:
                    (self.base / 'polaris-sig').write_text('MAA=')
                result = self.run_install()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(old.read_text(), 'old-install')
                if mode == 'missing':
                    subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.key), '-out',
                                    str(self.base / 'polaris-sig.der'), str(self.base / 'polaris-sha')], check=True)
                    subprocess.run(['openssl', 'base64', '-A', '-in', str(self.base / 'polaris-sig.der'), '-out',
                                    str(self.base / 'polaris-sig')], check=True)

    def test_release_key_is_synchronized(self):
        shell = (ROOT / 'scripts/install.sh').read_text()
        ps = (ROOT / 'scripts/install.ps1').read_text()
        shell_keys = re.findall(r"'([A-Za-z0-9+/=]+)'", re.search(r'(?ms)# RELEASE_PUBLIC_KEYS_BEGIN\n(.*?)\n# RELEASE_PUBLIC_KEYS_END', shell).group(1))
        ps_keys = re.findall(r"'([A-Za-z0-9+/=]+)'", re.search(r'(?ms)# RELEASE_PUBLIC_KEYS_BEGIN\n(.*?)\n# RELEASE_PUBLIC_KEYS_END', ps).group(1))
        expected = []
        for key_path in sorted((ROOT / 'internal/sysmgr/updater/releasekeys').glob('*.pem')):
            pem = key_path.read_text()
            expected.append(''.join(pem.splitlines()[1:-1]))
        self.assertEqual(shell_keys, ps_keys)
        self.assertEqual(shell_keys, expected)

    def test_npm_enterprise_registry_is_preserved(self):
        result = self.run_install(POLARIS_REGION='cn', MOCK_NPM_REGISTRY='https://registry.npmjs.org.corp.example')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.base / 'npm-config').exists())

    def test_uv_enterprise_config_is_preserved(self):
        config = self.home / '.config/uv/uv.toml'
        config.parent.mkdir(parents=True)
        config.write_text('[[index]]\nurl = "https://packages.corp.example/simple"\ndefault = true\n')
        result = self.run_install(POLARIS_REGION='cn')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(config.read_text(), '[[index]]\nurl = "https://packages.corp.example/simple"\ndefault = true\n')

    def test_explicit_uv_python_mirror_precedes_region_default(self):
        result = self.run_install(POLARIS_REGION='cn', UV_PYTHON_INSTALL_MIRROR='https://python.corp.example/builds')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        mirrors = (self.base / 'python-mirrors').read_text().splitlines()
        self.assertEqual(mirrors[0], 'https://python.corp.example/builds')

    def test_region_overrides_and_early_validation(self):
        result = self.run_install(POLARIS_REGION='cn')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        urls = (self.base / 'urls').read_text()
        self.assertNotIn('ipinfo.io', urls)
        self.assertIn('npmmirror.com/mirrors/node/v24.22.3/', urls)

        self.home = self.base / 'global home'
        self.home.mkdir()
        self.env['HOME'] = str(self.home)
        (self.base / 'urls').write_text('')
        result = self.run_install(POLARIS_REGION='global')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        urls = (self.base / 'urls').read_text()
        self.assertIn('https://nodejs.org/dist/v24.22.3/', urls)
        self.assertNotIn('npmmirror.com/mirrors/node/', urls)

        self.home = self.base / 'validation home'
        self.home.mkdir()
        self.env['HOME'] = str(self.home)
        (self.base / 'urls').write_text('')
        result = self.run_install(POLARIS_REGION='other')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.base / 'urls').read_text(), '')
        result = self.run_install(POLARIS_DOWNLOAD_BASE='http://mirror.example.test')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.base / 'urls').read_text(), '')

    def test_self_hosted_fixed_assets_work_when_upstream_metadata_is_unreachable(self):
        result = self.run_install(POLARIS_REGION='cn', POLARIS_DOWNLOAD_BASE='https://downloads.example.test/polaris',
                                  MOCK_MIRROR_ONLY='1')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        urls = (self.base / 'urls').read_text().splitlines()
        self.assertTrue(any('npmmirror.com/mirrors/node/v24.22.3/node-v24.22.3-linux-arm64.tar.gz' in url for url in urls))
        self.assertTrue(any('mirrors.aliyun.com/pypi/packages/96/72/' in url for url in urls))
        self.assertFalse(any('SHASUMS256.txt' in url or 'astral.sh' in url or 'api.github.com' in url for url in urls))
        self.assertFalse(any('github.com/astral-sh/uv/releases' in url for url in urls))

    def test_fixed_archive_failures_preserve_existing_node_and_uv(self):
        for env in ({'MOCK_BAD_NODE_SHA': '1'}, {'MOCK_NODE_ASSET_FAIL': '1'}, {'MOCK_NODE_FAIL': '1'}):
            with self.subTest(env=env):
                old_node = self.home / '.polaris/deps/node/keep.txt'
                old_node.parent.mkdir(parents=True, exist_ok=True)
                old_node.write_text('old-node')
                result = self.run_install(**env)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(old_node.read_text(), 'old-node')
                shutil.rmtree(old_node.parent)

        self.home = self.base / 'uvx missing home'
        self.home.mkdir()
        self.env['HOME'] = str(self.home)
        result = self.run_install(MOCK_UVX_MISSING='1')
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.home / '.local/bin/uvx').is_symlink())

    def test_preflight_failure_preserves_old_install(self):
        old = self.home / '.polaris/bin/polaris'
        old.parent.mkdir(parents=True)
        old.write_text('old-install')
        result = self.run_install(MOCK_PRECHECK_FAIL='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(old.read_text(), 'old-install')

    def test_macos_without_clt_continues_with_warning(self):
        executable(self.bin / 'git', 'exit 1\n')
        result = self.run_install(MOCK_OS='Darwin')
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.home / '.polaris/bin/polaris').exists())
        self.assertIn('Command Line', result.stdout)


if __name__ == '__main__':
    unittest.main()
