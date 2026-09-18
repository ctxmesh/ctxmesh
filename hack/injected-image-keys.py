import sys, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
bad = []
for key in ("controllerManager.injectedImages.collector",
            "controllerManager.injectedImages.discovery",
            "controllerManager.oboEgress.sidecarImage"):
    cur = doc
    for seg in key.split("."):
        cur = cur.get(seg) if isinstance(cur, dict) else None
    val = "" if cur is None else str(cur).strip()
    if val == "":
        bad.append(f"{key}|EMPTY")
    elif "/" not in val:
        bad.append(f"{key}|{val}")
print("\n".join(bad))
