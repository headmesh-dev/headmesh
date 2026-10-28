"""Preserve pasted transcripts and index commands without executing historical shell code."""

import argparse
import ast
import hashlib
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("transcripts", nargs="+", type=Path)
    args = parser.parse_args()
    source = ROOT / "sources"
    for directory in ("transcripts", "commands", "snippets", "recovered"):
        (source / directory).mkdir(parents=True, exist_ok=True)
    manifest, commands, created = [], [], []
    seen = {}
    recovered = {}
    for ordinal, input_path in enumerate(args.transcripts, 1):
        data = input_path.read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        item = {"original_path": str(input_path), "sha256": digest, "lines": len(data.splitlines())}
        if digest in seen:
            item["duplicate_of"] = seen[digest]
            manifest.append(item)
            continue
        filename = f"transcript-{ordinal:02}.txt"
        seen[digest] = filename
        item["saved_as"] = filename
        (source / "transcripts" / filename).write_bytes(data)
        manifest.append(item)
        text = data.decode("utf-8")
        lines = text.splitlines()
        for index, line in enumerate(lines):
            if line in ("Created a file", "Edited a file") and index + 1 < len(lines):
                created.append({"transcript": filename, "line": index + 1, "event": line, "file": lines[index + 1]})
        for match in re.finditer(r"^bash\s*\n(.*?)(?=^Output\s*$|^Read a file\s*$|^Created a file\s*$|^Edited a file\s*$|^Request\s*$|\Z)", text, re.M | re.S):
            body = match.group(1).strip() + "\n"
            identifier = f"{ordinal:02}-{len(commands) + 1:03}"
            saved = f"commands/{identifier}.sh"
            (source / saved).write_text(body)
            command = {
                "id": identifier, "transcript": filename,
                "line": text[:match.start()].count("\n") + 1,
                "saved_as": saved, "sha256": hashlib.sha256(body.encode()).hexdigest(),
                "summary": body.splitlines()[0][:180],
            }
            commands.append(command)
            for snippet_index, snippet in enumerate(re.finditer(r"<<\s*(['\"]?)([A-Za-z_]\w*)\1[^\n]*\n(.*?)^\2\s*$", body, re.M | re.S), 1):
                header = body[:snippet.start()].rsplit("\n", 1)[-1]
                content = snippet.group(3)
                suffix = ".py" if "python" in header else ".txt"
                if ".js" in header:
                    suffix = ".js"
                if ".astro" in header:
                    suffix = ".astro"
                dest = f"snippets/{identifier}-{snippet_index:02}{suffix}"
                (source / dest).write_text(content)
                if suffix != ".py":
                    continue
                try:
                    tree = ast.parse(content)
                except SyntaxError:
                    continue
                for node in ast.walk(tree):
                    if not isinstance(node, ast.Constant) or not isinstance(node.value, str):
                        continue
                    value = node.value
                    if value.startswith("def constellation(") and len(value) > len(recovered.get("constellation.py", "")):
                        recovered["constellation.py"] = value
                    elif value.startswith("def mark_D4("):
                        recovered["mark_D4.py"] = value.split("\n\nMARKS =", 1)[0] + "\n"
                    elif 'head is the hub where links cross' in value:
                        recovered["mark_A1_final.py"] = 'def mark_A1(paint="grad", pid="a1"):\n' + value + "\n    return defs, body\n"
    if "constellation.py" in recovered:
        recovered["constellation.py"] = recovered["constellation.py"].replace(
            "mx_, my_ = (left[0] + right[0]) / 2, (left[1] + right[1]) / 2 - fs * 1.5",
            "mx_ = x + wa + gap / 2\n        my_ = left[1] + (right[1] - left[1]) * ((mx_ - left[0]) / (right[0] - left[0])) - fs * 1.4",
        )
    for name, content in recovered.items():
        (source / "recovered" / name).write_text(content)
    report = {"transcripts": manifest, "commands": commands, "file_events": created,
              "recovered_functions": sorted(recovered),
              "initial_file_bodies_absent": ["textpath.py", "marks.py", "preview.py", "render.js", "build.py", "global.css"]}
    (source / "inventory.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"unique_transcripts": len(seen), "commands": len(commands), "recovered_functions": sorted(recovered)}))


if __name__ == "__main__":
    main()
