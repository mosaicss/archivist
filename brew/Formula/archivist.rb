# Reference copy of the Homebrew formula.
#
# The authoritative formula lives in the mosaic-finance/homebrew-tap repo
# (github.com/mosaic-finance/homebrew-tap) and is auto-updated by goreleaser's
# `brews:` config on every tagged release. The copy below is a template kept
# here for documentation and review purposes only.
#
# Operator must:
# 1. Create the public repo: github.com/mosaic-finance/homebrew-tap
# 2. Add HOMEBREW_TAP_GITHUB_TOKEN repo secret to mosaicss/archivist with
#    `contents: write` access on the tap repo.
# See RELEASE_OPS_36_11.md for the full checklist.

class Archivist < Formula
  desc "Archivist CLI for Mosaic filings research"
  homepage "https://mosaic-finance.com/products/archivist-cli"
  license "Apache-2.0"
  version "GORELEASER_WILL_REPLACE"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/mosaicss/archivist/releases/download/v#{version}/archivist_v#{version}_darwin_arm64.tar.gz"
      sha256 "GORELEASER_WILL_REPLACE"
    else
      url "https://github.com/mosaicss/archivist/releases/download/v#{version}/archivist_v#{version}_darwin_amd64.tar.gz"
      sha256 "GORELEASER_WILL_REPLACE"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/mosaicss/archivist/releases/download/v#{version}/archivist_v#{version}_linux_arm64.tar.gz"
      sha256 "GORELEASER_WILL_REPLACE"
    else
      url "https://github.com/mosaicss/archivist/releases/download/v#{version}/archivist_v#{version}_linux_amd64.tar.gz"
      sha256 "GORELEASER_WILL_REPLACE"
    end
  end

  def install
    bin.install "archivist"
  end

  def post_install
    # Fetch and install Claude Code skill bundle (silent if ~/.claude/skills/ doesn't exist)
    skill_dir = File.expand_path("~/.claude/skills/archivist")
    if File.directory?(File.expand_path("~/.claude/skills"))
      system "mkdir", "-p", skill_dir
      system "curl", "-fsSL", "-o", "/tmp/archivist-skill.tar.gz",
        "https://github.com/mosaicss/archivist/releases/download/v#{version}/archivist_v#{version}_skill-bundle.tar.gz"
      system "tar", "-xzf", "/tmp/archivist-skill.tar.gz", "-C", skill_dir, "--strip-components=0"
      system "rm", "-f", "/tmp/archivist-skill.tar.gz"
    end
    # Record install channel
    system "mkdir", "-p", File.expand_path("~/.archivist")
    system "sh", "-c", "echo brew > ~/.archivist/install-channel"
  end

  test do
    output = shell_output("#{bin}/archivist version")
    assert_match version.to_s, output
  end
end
