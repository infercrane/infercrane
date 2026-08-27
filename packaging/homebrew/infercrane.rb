class Infercrane < Formula
  desc "Operate self-hosted models behind one OpenAI-compatible endpoint"
  homepage "https://github.com/infercrane/infercrane"
  # Generated release formulae substitute these template values from checksums.txt.
  version "RELEASE_VERSION"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "RELEASE_BASE_URL/infercrane_RELEASE_VERSION_darwin_arm64.tar.gz"
      sha256 "RELEASE_DARWIN_ARM64_SHA256"
    else
      url "RELEASE_BASE_URL/infercrane_RELEASE_VERSION_darwin_amd64.tar.gz"
      sha256 "RELEASE_DARWIN_AMD64_SHA256"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "RELEASE_BASE_URL/infercrane_RELEASE_VERSION_linux_arm64.tar.gz"
      sha256 "RELEASE_LINUX_ARM64_SHA256"
    else
      url "RELEASE_BASE_URL/infercrane_RELEASE_VERSION_linux_amd64.tar.gz"
      sha256 "RELEASE_LINUX_AMD64_SHA256"
    end
  end

  def install
    bin.install "infercrane"
    generate_completions_from_executable(bin/"infercrane", "completion")
  end

  test do
    assert_match version.to_s.split("-").first, shell_output("#{bin}/infercrane version")
  end
end
