class Infercrane < Formula
  desc "Production inference without the platform engineering"
  homepage "https://github.com/infercrane/infercrane"
  version "0.1.0-rc.1"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/infercrane/infercrane/releases/download/v0.1.0-rc.1/infercrane_0.1.0-rc.1_darwin_arm64.tar.gz"
      sha256 "REPLACE_WITH_RELEASE_CHECKSUM"
    else
      url "https://github.com/infercrane/infercrane/releases/download/v0.1.0-rc.1/infercrane_0.1.0-rc.1_darwin_amd64.tar.gz"
      sha256 "REPLACE_WITH_RELEASE_CHECKSUM"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/infercrane/infercrane/releases/download/v0.1.0-rc.1/infercrane_0.1.0-rc.1_linux_arm64.tar.gz"
      sha256 "REPLACE_WITH_RELEASE_CHECKSUM"
    else
      url "https://github.com/infercrane/infercrane/releases/download/v0.1.0-rc.1/infercrane_0.1.0-rc.1_linux_amd64.tar.gz"
      sha256 "REPLACE_WITH_RELEASE_CHECKSUM"
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
