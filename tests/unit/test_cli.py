from typer.testing import CliRunner

from infercrane.cli.app import app


def test_cli_target_deploy_status_and_delete(tmp_path, monkeypatch):
    monkeypatch.setenv("INFERCRANE_STATE_DIR", str(tmp_path))
    runner = CliRunner()

    added = runner.invoke(
        app,
        ["target", "add", "gpu-a", "--url", "http://127.0.0.1:8101", "--runtime", "vllm"],
    )
    assert added.exit_code == 0, added.output

    deployed = runner.invoke(
        app,
        ["deploy", "Qwen/Qwen3-8B", "--name", "qwen-prod", "--targets", "gpu-a"],
    )
    assert deployed.exit_code == 0, deployed.output

    listed = runner.invoke(app, ["deployments"])
    assert listed.exit_code == 0
    assert "qwen-prod" in listed.output

    routed = runner.invoke(app, ["route", "qwen-prod", "--strategy", "cache-aware"])
    assert routed.exit_code == 0, routed.output
    assert "cache-aware" in routed.output

    deleted = runner.invoke(app, ["delete", "qwen-prod"])
    assert deleted.exit_code == 0
    assert "qwen-prod" not in runner.invoke(app, ["deployments"]).output
