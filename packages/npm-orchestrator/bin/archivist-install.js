#!/usr/bin/env node
// Archivist CLI npm orchestrator — downloads and installs the platform binary.
//
// Usage: npx -y @mosaic-finance/archivist install
//
// Pinned installs are recommended for reproducibility:
//   npx -y @mosaic-finance/archivist@x.y.z install
//
// Zero runtime dependencies. All SHA256 hashes are injected by the release
// workflow at publish time (placeholders below are replaced by sed before npm publish).

'use strict';

const { execFileSync, execSync } = require('child_process');
const fs = require('fs');
const https = require('https');
const os = require('os');
const path = require('path');
const crypto = require('crypto');

const PKG = require('../package.json');
const VERSION = PKG.version;
const REPO = 'mosaicss/archivist';

// SHA256 hashes injected by the release workflow.
// Placeholders follow the pattern: SHA256_<PLATFORM_UPPER>
// where PLATFORM_UPPER = DARWIN_ARM64, DARWIN_AMD64, LINUX_ARM64, LINUX_AMD64, WINDOWS_AMD64
const HASHES = {
  darwin_arm64:   'SHA256_DARWIN_ARM64',
  darwin_amd64:   'SHA256_DARWIN_AMD64',
  linux_arm64:    'SHA256_LINUX_ARM64',
  linux_amd64:    'SHA256_LINUX_AMD64',
  windows_amd64:  'SHA256_WINDOWS_AMD64',
};

function detectPlatform() {
  const plat = process.platform;
  const arch = process.arch;
  if (plat === 'darwin' && arch === 'arm64') return 'darwin_arm64';
  if (plat === 'darwin' && arch === 'x64')   return 'darwin_amd64';
  if (plat === 'linux'  && arch === 'arm64') return 'linux_arm64';
  if (plat === 'linux'  && arch === 'x64')   return 'linux_amd64';
  if (plat === 'win32'  && arch === 'x64')   return 'windows_amd64';
  throw new Error(`Unsupported platform: ${plat}/${arch}. Download manually from https://github.com/${REPO}/releases`);
}

function archiveFilename(platform) {
  const ext = platform.startsWith('windows') ? 'zip' : 'tar.gz';
  return `archivist_v${VERSION}_${platform}.${ext}`;
}

function downloadFile(url, dest) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(dest);
    function get(u) {
      https.get(u, (res) => {
        if (res.statusCode === 301 || res.statusCode === 302) {
          return get(res.headers.location);
        }
        if (res.statusCode !== 200) {
          reject(new Error(`HTTP ${res.statusCode} downloading ${u}`));
          return;
        }
        res.pipe(file);
        file.on('finish', () => file.close(resolve));
      }).on('error', reject);
    }
    get(url);
  });
}

function sha256File(filepath) {
  const hash = crypto.createHash('sha256');
  hash.update(fs.readFileSync(filepath));
  return hash.digest('hex');
}

function resolveInstallDir() {
  // Prefer $npm_config_prefix/bin; fall back to ~/bin or ~/.local/bin
  if (process.env.npm_config_prefix) {
    return path.join(process.env.npm_config_prefix, 'bin');
  }
  try {
    const npmBin = execSync('npm bin -g', { encoding: 'utf8' }).trim();
    if (npmBin) return npmBin;
  } catch (_) { /* ignore */ }
  const localBin = path.join(os.homedir(), '.local', 'bin');
  fs.mkdirSync(localBin, { recursive: true });
  return localBin;
}

function extractArchive(archive, dest, platform) {
  if (platform.startsWith('windows')) {
    // unzip is not always available on Windows; try PowerShell
    try {
      execFileSync('powershell', [
        '-Command',
        `Expand-Archive -Path "${archive}" -DestinationPath "${dest}" -Force`,
      ]);
    } catch (_) {
      execFileSync('unzip', ['-o', archive, '-d', dest]);
    }
  } else {
    execFileSync('tar', ['-xzf', archive, '-C', dest]);
  }
}

function installSkill() {
  const skillBase = path.join(os.homedir(), '.claude', 'skills');
  if (!fs.existsSync(skillBase)) return;

  const skillDir = path.join(skillBase, 'archivist');
  const skillArchive = `archivist_v${VERSION}_skill-bundle.tar.gz`;
  const skillUrl = `https://github.com/${REPO}/releases/download/v${VERSION}/${skillArchive}`;
  const tmpSkill = path.join(os.tmpdir(), skillArchive);

  try {
    console.log('Installing Claude Code skill...');
    downloadFile(skillUrl, tmpSkill).then(() => {
      fs.mkdirSync(skillDir, { recursive: true });
      execFileSync('tar', ['-xzf', tmpSkill, '-C', skillDir]);
      fs.unlinkSync(tmpSkill);
      console.log(`Skill installed at ${skillDir}`);
    }).catch(() => {
      // Silent skip if skill bundle download fails
    });
  } catch (_) { /* silent */ }
}

function recordChannel() {
  const archivistDir = path.join(os.homedir(), '.archivist');
  fs.mkdirSync(archivistDir, { recursive: true });
  fs.writeFileSync(path.join(archivistDir, 'install-channel'), 'npm', 'utf8');
}

async function main() {
  const args = process.argv.slice(2);
  if (args[0] !== 'install') {
    console.log('Usage: npx -y @mosaic-finance/archivist install');
    process.exit(0);
  }

  const platform = detectPlatform();
  const expectedHash = HASHES[platform];

  // If the hash is still a placeholder (not injected by release workflow), fail loud
  if (expectedHash.startsWith('SHA256_')) {
    console.error('Error: This package was not built by the official release pipeline (SHA256 hashes are placeholders).');
    console.error(`Download manually from https://github.com/${REPO}/releases`);
    process.exit(1);
  }

  const filename = archiveFilename(platform);
  const url = `https://github.com/${REPO}/releases/download/v${VERSION}/${filename}`;
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'archivist-'));
  const archivePath = path.join(tmpDir, filename);

  try {
    console.log(`Downloading archivist v${VERSION} for ${platform}...`);
    await downloadFile(url, archivePath);

    const actualHash = sha256File(archivePath);
    if (actualHash !== expectedHash) {
      console.error(`SHA256 mismatch for ${filename}`);
      console.error(`  expected: ${expectedHash}`);
      console.error(`  got:      ${actualHash}`);
      process.exit(1);
    }
    console.log('SHA256 verified.');

    extractArchive(archivePath, tmpDir, platform);

    const binaryName = platform.startsWith('windows') ? 'archivist.exe' : 'archivist';
    const binaryPath = path.join(tmpDir, binaryName);
    if (!fs.existsSync(binaryPath)) {
      throw new Error(`Binary not found in archive at ${binaryPath}`);
    }

    const installDir = resolveInstallDir();
    fs.mkdirSync(installDir, { recursive: true });
    const dest = path.join(installDir, binaryName);
    fs.copyFileSync(binaryPath, dest);
    if (!platform.startsWith('windows')) {
      fs.chmodSync(dest, 0o755);
    }

    console.log(`Installed: ${dest}`);
    recordChannel();
    installSkill();

    console.log('');
    console.log(`Archivist v${VERSION} installed successfully.`);
    console.log('');
    console.log('Next steps:');
    console.log('  archivist --help');
    console.log('  archivist auth login');
    console.log('');
    console.log('Docs: https://mosaic-finance.com/guides/archivist-cli');
  } finally {
    try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch (_) {}
  }
}

main().catch((err) => {
  console.error('Install failed:', err.message);
  process.exit(1);
});
