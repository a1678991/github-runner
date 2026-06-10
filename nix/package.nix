{
  lib,
  buildGoModule,
  qemu,
  cdrkit,
  src,
}:
buildGoModule {
  pname = "github-qemu-runner";
  version = "0-unstable-2026-06-10";
  inherit src;
  vendorHash = "sha256-g+yaVIx4jxpAQ/+WrGKxhVeliYx7nLQe/zsGpxV4Fn4=";
  subPackages = [ "cmd/github-qemu-runner" ];

  # Patch the go directive to match nixpkgs' go version so GOTOOLCHAIN=local
  # does not reject the build (go.mod is set by the host's newer toolchain).
  prePatch = ''
    sed -i 's/^go [0-9.]*$/go 1.26.3/' go.mod
  '';

  # qemu-img and genisoimage let the integration-gated tests run in the
  # sandbox instead of skipping.
  nativeCheckInputs = [
    qemu
    cdrkit
  ];

  # subPackages restricts the check phase to cmd/ only; override to test ./...
  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';

  meta = {
    description = "Ephemeral GitHub Actions self-hosted runners in disposable QEMU/KVM VMs";
    homepage = "https://github.com/a1678991/github-qemu-runner";
    license = lib.licenses.mit;
    platforms = [ "x86_64-linux" ];
    mainProgram = "github-qemu-runner";
  };
}
