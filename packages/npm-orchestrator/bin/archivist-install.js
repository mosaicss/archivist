#!/usr/bin/env node
// Archivist CLI npm orchestrator — downloads and installs the platform binary.
//
// Usage: npx -y @mosaic-finance/archivist install
//
// Pinned installs are recommended for reproducibility:
//   npx -y @mosaic-finance/archivist@x.y.z install
//
// Zero runtime dependencies. All SHA256 hashes are injected by the release
// workflow at publish time (placeholders below are replaced by sed before npm
// publish) from the release's signed SHA256SUMS, so the archive and the skill
// bundle are checked against hashes shipped inside the package itself.

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
// Placeholders follow the pattern SHA256_<ASSET_UPPER>, where ASSET_UPPER is
// the asset name after archivist_v<ver>_ without its extension: DARWIN_ALL
// (one universal macOS archive), LINUX_ARM64, LINUX_AMD64, WINDOWS_AMD64 and
// SKILL_BUNDLE.
const HASHES = {
  darwin_all:     'SHA256_DARWIN_ALL',
  linux_arm64:    'SHA256_LINUX_ARM64',
  linux_amd64:    'SHA256_LINUX_AMD64',
  windows_amd64:  'SHA256_WINDOWS_AMD64',
};
const SKILL_BUNDLE_HASH = 'SHA256_SKILL_BUNDLE';

// isPlaceholder: the release workflow did not fill this hash.
function isPlaceholder(hash) {
  return !/^[0-9a-f]{64}$/.test(hash);
}

function detectPlatform() {
  const plat = process.platform;
  const arch = process.arch;
  // One universal binary (arm64 and amd64) per release.
  if (plat === 'darwin' && (arch === 'arm64' || arch === 'x64')) return 'darwin_all';
  if (plat === 'linux'  && arch === 'arm64') return 'linux_arm64';
  if (plat === 'linux'  && arch === 'x64')   return 'linux_amd64';
  if (plat === 'win32'  && arch === 'x64')   return 'windows_amd64';
  throw new Error(`Unsupported platform: ${plat}/${arch}. Download manually from https://github.com/${REPO}/releases`);
}

function archiveFilename(platform) {
  const ext = platform.startsWith('windows') ? 'zip' : 'tar.gz';
  return `archivist_v${VERSION}_${platform}.${ext}`;
}

// downloadFile fetches an https:// URL (following up to 10 https://
// redirects) into dest.
function downloadFile(url, dest) {
  return new Promise((resolve, reject) => {
    function get(u, redirects) {
      let parsed;
      try {
        parsed = new URL(u);
      } catch (err) {
        reject(err);
        return;
      }
      if (parsed.protocol !== 'https:') {
        reject(new Error(`refusing a non-https download: ${u}`));
        return;
      }
      https.get(parsed, (res) => {
        if ([301, 302, 303, 307, 308].includes(res.statusCode) && res.headers.location) {
          res.resume();
          if (redirects >= 10) {
            reject(new Error(`too many redirects downloading ${url}`));
            return;
          }
          get(new URL(res.headers.location, parsed).toString(), redirects + 1);
          return;
        }
        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error(`HTTP ${res.statusCode} downloading ${u}`));
          return;
        }
        const file = fs.createWriteStream(dest);
        file.on('error', reject);
        res.on('error', reject);
        res.pipe(file);
        file.on('finish', () => file.close((err) => (err ? reject(err) : resolve())));
      }).on('error', reject);
    }
    get(url, 0);
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

// installSkill downloads the skill bundle into tmpDir and unpacks it into
// ~/.claude/skills/archivist only when it matches SKILL_BUNDLE_HASH. Any
// failure skips the skill with a note; the binary install stands.
async function installSkill(tmpDir) {
  const skillBase = path.join(os.homedir(), '.claude', 'skills');
  if (!fs.existsSync(skillBase)) return;

  const skillDir = path.join(skillBase, 'archivist');
  const skillArchive = `archivist_v${VERSION}_skill-bundle.tar.gz`;
  const skillUrl = `https://github.com/${REPO}/releases/download/v${VERSION}/${skillArchive}`;
  const tmpSkill = path.join(tmpDir, skillArchive);
  const skipped = 'Note: the Claude Code skill bundle could not be installed; skipped. Run: archivist update --skill';

  if (isPlaceholder(SKILL_BUNDLE_HASH)) {
    console.log(skipped);
    return;
  }
  try {
    console.log('Installing Claude Code skill...');
    await downloadFile(skillUrl, tmpSkill);
    if (sha256File(tmpSkill) !== SKILL_BUNDLE_HASH) {
      console.log(skipped);
      return;
    }
    fs.mkdirSync(skillDir, { recursive: true });
    execFileSync('tar', ['-xzf', tmpSkill, '-C', skillDir]);
    console.log(`Skill installed at ${skillDir}`);
  } catch (_) {
    console.log(skipped);
  }
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
  if (isPlaceholder(expectedHash)) {
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
    await installSkill(tmpDir);

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
