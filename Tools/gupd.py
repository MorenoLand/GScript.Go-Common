#!/usr/bin/env python3
"""Makes update packages (.gupd) for a GameServer: the login server's "Install Packages" window, or a game server's own.

A package folder (the output) holds everything a server needs and nothing else:

  <out>/basepackage.gupd        the package the client asks for first; it names every package of the folder (SUBPACKAGE)
  <out>/<name>.gupd             one manifest per package: its name, version, mode, platform, description and FILE lines
  <out>/files/<file>            a copy of every file the packages list, under its bare name (the FILE line says where the client
                                stores it; the server finds a file by its bare name wherever it is)
  <out>/.state/<name>.json      what the tool needs to know whether a file changed (never read by a server or a client)

The folder goes into the server's world folder as world/packages/ (and the .state folder with it: the next run continues from it),
and two lines of config/foldersconfig.txt make the server find the manifests and the copies (`foldersconfig` prints them).

  gupd.py make --out <folder> --file starter.gupd --name "Starter files" [--mode optional|required|base] [--platform any]
               [--description TEXT | --description-file FILE] [--place auto|keep|<folder>/] [--checksum]
               [--exclude <pattern>]... [--base-name TEXT] <source folder>
  gupd.py build --out <folder> <source folder>     every sub-folder with a package.txt is a package
  gupd.py remove --out <folder> --file starter.gupd
  gupd.py foldersconfig [--prefix packages] [--merge <foldersconfig.txt>] [--out <folder> --world <world folder>]
  gupd.py check <file.gupd>

--place auto needs client_files.py next to this file: a module with cache_path(lower-case file name), the place the client keeps a
file of that name. Without it use --place keep or --place <folder>/.

Standard library only; Python 3.8 or newer.
"""
import argparse
import fnmatch
import hashlib
import json
import os
import re
import shutil
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
try:
    import client_files  # noqa: E402  (cache_path: where the client keeps a file of a name)
except ImportError:
    client_files = None

HEADER = "GRPKG001"
BASE_PACKAGE = "basepackage.gupd"
MODES = ("optional", "required", "base")
STATE_FOLDER = ".state"
COPIES_FOLDER = "files"
# The block of foldersconfig.txt this tool writes (--merge); everything outside it stays as it is.
BLOCK_BEGIN = "# BEGIN update packages (Tools/gupd.py writes this block)"
BLOCK_END = "# END update packages"
# Files a client refuses in a game server's package and that a login server's package would install as programs: never packaged.
PROGRAM_EXTENSIONS = (".exe", ".lexe", ".bat", ".com")
PROGRAM_FILE = "graal6.exe"
# Characters a FILE line's folder may have: letters, digits, '_' and '-', in segments joined by '/'.
FOLDER_SEGMENT = re.compile(r"^[A-Za-z0-9_-]+$")
# Characters a file name may not have: the manifest is read as bytes by the client (code page 1252) and as UTF-8 by the server, so
# only ASCII names mean the same to both; these are not allowed in a Windows file name, where the desktop client stores the file.
FORBIDDEN_NAME_CHARACTERS = set('\\/:*?"<>|')


class PackageError(Exception):
    """A package the tool refuses to write, with the reason."""


def cache_path(lower):
    if client_files is None:
        raise PackageError("--place auto needs client_files.py next to gupd.py (use --place keep or --place <folder>/ without it)")
    return client_files.cache_path(lower)


def check_file_name(name):
    """Refuses a bare file name the client and the server would not treat alike. Returns the name in lower case."""
    if not name:
        raise PackageError("a file has an empty name")
    if name != name.strip():
        raise PackageError(f"{name!r}: blanks at the start or end of a name (the client cuts them off, the server does not)")
    for character in name:
        if not (" " <= character <= "~"):
            raise PackageError(f"{name!r}: only ASCII letters, digits and punctuation in a file name (the client reads the manifest "
                               "as code page 1252, the server as UTF-8: any other character means two different names)")
        if character in FORBIDDEN_NAME_CHARACTERS:
            raise PackageError(f"{name!r}: the character {character!r} is not allowed in a file name")
    lower = name.lower()
    extension = os.path.splitext(lower)[1]
    if extension in PROGRAM_EXTENSIONS or lower == PROGRAM_FILE:
        raise PackageError(f"{name}: programs are never packaged (a game server's package cannot bring one, and a login server's "
                           "would be installed as a program)")
    if extension == ".gupd":
        raise PackageError(f"{name}: a package cannot list a .gupd file (name it as a sub-package of the base package instead)")
    return lower


def check_folder(folder):
    """Refuses a FILE line's folder that the client or the server would not take ('' is the user folder itself)."""
    if folder == "":
        return
    if folder.startswith("/") or folder.endswith("/") or "//" in folder:
        raise PackageError(f"{folder!r}: a place is a relative folder like levels/images, without '/' at its start or end")
    for segment in folder.split("/"):
        if not FOLDER_SEGMENT.match(segment):
            raise PackageError(f"{folder!r}: a folder of a place may have only letters, digits, '_' and '-' (the client refuses a "
                               "'.' and the server ':', '.' and '..')")


def place_of(relative, place):
    """Where the client stores a file of the source folder, relative to its user folder (the FILE line). The name is in lower case:
    the client keeps files under lower-case names, finds them by lower-case name, and the server finds them in any case.

    auto: the client's own place for a file of that name, where it also looks for the file on any server;
    keep: the file's path in the source folder; <folder>/: that folder."""
    folder, name = os.path.split(relative.replace(os.sep, "/"))
    lower = check_file_name(name)
    if place == "auto":
        result = cache_path(lower)
    elif place == "keep":
        result = (folder + "/" if folder else "") + lower
    else:
        result = place.rstrip("/") + "/" + lower
    check_folder(os.path.dirname(result))
    return result


def sha256_of(path):
    digest = hashlib.sha256()
    with open(path, "rb") as file:
        for block in iter(lambda: file.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def check_package_file_name(file_name):
    """A package's own file: a bare lower-case .gupd name the client stores and the server finds."""
    if file_name != os.path.basename(file_name) or not file_name.endswith(".gupd"):
        raise PackageError(f"{file_name!r}: a package's file is a bare name ending in .gupd")
    stem = file_name[: -len(".gupd")]
    if file_name != file_name.lower() or not FOLDER_SEGMENT.match(stem):
        raise PackageError(f"{file_name!r}: a package's file name has only lower-case letters, digits, '_' and '-' before .gupd "
                           "(the client asks for sub-packages in lower case)")
    return stem


def check_text(value, what):
    """A NAME or a description line: one line, in code page 1252 (what the client reads)."""
    if "\n" in value or "\r" in value:
        raise PackageError(f"{what}: one line only")
    try:
        value.encode("cp1252")
    except UnicodeEncodeError:
        raise PackageError(f"{what}: {value!r} has characters the client cannot show (it reads code page 1252)") from None


def manifest_text(name, version, platform, mode, description, checksum, subpackages, files):
    """The manifest's lines in the order the client needs: PLATFORM before every SUBPACKAGE and FILE (the client decides with the
    platform it has read so far whether it takes such a line)."""
    lines = [HEADER, "NAME " + name, "VERSION " + format_version(version), "PLATFORM " + platform]
    if mode != "optional":
        lines.append("MODE " + mode)
    if checksum:
        lines.append("USECHECKSUM")
    if description:
        lines.append("DESCRIPTION")
        lines.extend(description)
        lines.append("DESCRIPTIONEND")
    lines.extend("SUBPACKAGE " + subpackage for subpackage in subpackages)
    lines.extend("FILE " + file for file in files)
    return "\n".join(lines) + "\n"


def format_version(version):
    return str(int(version)) if float(version).is_integer() else repr(float(version))


def read_manifest(path):
    """The lines of a manifest the way the client reads them: None when the first line is not GRPKG001."""
    with open(path, "rb") as file:
        lines = file.read().decode("cp1252").replace("\r\n", "\n").split("\n")
    if not lines or lines[0] != HEADER:
        return None
    result = {"name": "", "version": 0.0, "platform": "any", "mode": "optional", "accounts": [], "subpackages": [], "files": [],
              "description": None, "usechecksum": False, "protectoverwrite": False, "ismainexecutable": False, "flags": [],
              "ignored": []}
    index = 1
    while index < len(lines):
        line = lines[index]
        if line.startswith("NAME "):
            result["name"] = line[5:]
        elif line.startswith("FLAG "):
            result["flags"].append(line[5:])
        elif line.startswith("VERSION "):
            result["version"] = to_double(line[8:])
        elif line.startswith("PLATFORM "):
            result["platform"] = line[9:]
        elif line.startswith("ACCOUNTS "):
            result["accounts"] = [account for account in line[9:].split(",") if account]
        elif line.startswith("MODE "):
            result["mode"] = line[5:]
        elif line.startswith("SUBPACKAGE "):
            result["subpackages"].append(line[11:])
        elif line.startswith("FILE "):
            result["files"].append(line[5:])
        elif line == "DESCRIPTION":
            description = []
            index += 1
            while index < len(lines) and lines[index] != "DESCRIPTIONEND":
                description.append(lines[index])
                index += 1
            result["description"] = description
        elif line == "ISMAINEXECUTABLE":
            result["ismainexecutable"] = True
        elif line == "USECHECKSUM":
            result["usechecksum"] = True
        elif line == "PROTECTOVERWRITE":
            result["protectoverwrite"] = True
        elif line.strip():
            result["ignored"].append(line)
        index += 1
    return result


def to_double(text):
    """The leading number of a text, 0 when there is none (as the client's ToDouble reads VERSION)."""
    match = re.match(r"\s*[-+]?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?", text)
    return float(match.group(0)) if match else 0.0


def load_state(out, stem):
    path = os.path.join(out, STATE_FOLDER, stem + ".json")
    if not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as file:
        return json.load(file)


def save_state(out, stem, state):
    folder = os.path.join(out, STATE_FOLDER)
    os.makedirs(folder, exist_ok=True)
    temporary = os.path.join(folder, stem + ".json.new")
    with open(temporary, "w", encoding="utf-8") as file:
        json.dump(state, file, indent=1, sort_keys=True)
        file.write("\n")
    os.replace(temporary, os.path.join(folder, stem + ".json"))


def write_text(path, text):
    temporary = path + ".new"
    with open(temporary, "wb") as file:
        file.write(text.encode("cp1252"))
    os.replace(temporary, path)


def previous_version(out, file_name, state):
    """The version the package had: from the tool's state, else from a manifest that is already there (made by hand), else 0."""
    if state is not None:
        return state["version"]
    path = os.path.join(out, file_name)
    if os.path.exists(path):
        manifest = read_manifest(path)
        if manifest is not None:
            return manifest["version"]
    return 0


def gather(source, excludes):
    """The files of the source folder, as (relative path, full path), sorted; hidden files and excluded names are reported and left out."""
    if not os.path.isdir(source):
        raise PackageError(f"{source}: no such folder")
    found, skipped = [], []
    for folder, folders, names in os.walk(source):
        folders[:] = sorted(name for name in folders if not name.startswith("."))
        for name in sorted(names):
            full = os.path.join(folder, name)
            relative = os.path.relpath(full, source).replace(os.sep, "/")
            if name.startswith("."):
                skipped.append(f"{relative} (a hidden file)")
            elif any(fnmatch.fnmatch(relative, pattern) or fnmatch.fnmatch(name, pattern) for pattern in excludes):
                skipped.append(f"{relative} (--exclude)")
            elif os.path.islink(full) or not os.path.isfile(full):
                skipped.append(f"{relative} (not a plain file)")
            elif os.path.getsize(full) == 0:
                skipped.append(f"{relative} (empty: the server leaves empty files out of a package)")
            else:
                found.append((relative, full))
    if not found:
        raise PackageError(f"{source}: no files to package")
    return found, skipped


def other_packages_names(out, stem):
    """Bare names (lower case) that the other packages of the folder list, with the package that lists each."""
    names = {}
    state_folder = os.path.join(out, STATE_FOLDER)
    if not os.path.isdir(state_folder):
        return names
    for entry in sorted(os.listdir(state_folder)):
        if not entry.endswith(".json") or entry[: -len(".json")] in (stem, "basepackage"):
            continue
        with open(os.path.join(state_folder, entry), encoding="utf-8") as file:
            for item in json.load(file).get("files", []):
                names[item["place"].rsplit("/", 1)[-1]] = entry[: -len(".json")] + ".gupd"
    return names


def make(out, file_name, name, source, mode="optional", platform="any", description=(), place="auto", checksum=False, excludes=(),
         base_name="Packages", now=None, log=print, taken=None, name_in_base=True):
    """Writes (or brings up to date) one package of the folder and the base package that names it. Returns its version. taken: the
    bare names the folder's other packages list, with their package (default: what their state says). name_in_base: whether to
    write the base package now (build writes it once, after all its packages)."""
    stem = check_package_file_name(file_name)
    if file_name == BASE_PACKAGE:
        raise PackageError("basepackage.gupd is the tool's own: it names the packages you make")
    if mode not in MODES:
        raise PackageError(f"--mode {mode}: one of {', '.join(MODES)}")
    if not re.match(r"^[a-z0-9]+$", platform):
        raise PackageError(f"--platform {platform}: a platform name such as any or win")
    if not name.strip():
        raise PackageError("--name: a package needs a name (the window lists it by name)")
    check_text(name, "--name")
    check_text(base_name, "--base-name")
    description = list(description)
    for line in description:
        check_text(line, "the description")
        if line == "DESCRIPTIONEND":
            raise PackageError("the description: a line DESCRIPTIONEND would end it early")
    if place not in ("auto", "keep"):
        check_folder(place.rstrip("/"))
    now = int(time.time()) if now is None else int(now)

    files, skipped = gather(source, excludes)
    for line in skipped:
        log(f"skipped {line}")
    entries = []
    by_name = {}
    taken_elsewhere = other_packages_names(out, stem) if taken is None else taken
    for relative, full in files:
        target = place_of(relative, place)
        bare = target.rsplit("/", 1)[-1]
        if bare in by_name:
            raise PackageError(f"{relative} and {by_name[bare]}: two files named {bare} (the server finds a file by its bare name "
                               "and the client by its lower-case name: one of them would never arrive)")
        if bare in taken_elsewhere:
            raise PackageError(f"{relative}: {taken_elsewhere[bare]} of this folder lists a file named {bare} too (the server finds "
                               "only one file of that name)")
        by_name[bare] = relative
        if place == "keep" and client_files is not None and target != client_files.cache_path(bare):
            log(f"note: {target} is not the client's own place for {bare} ({client_files.cache_path(bare)}): the client finds it "
                "there only while it is on this server")
        entries.append({"place": target, "sha256": sha256_of(full), "size": os.path.getsize(full), "source": full})

    state = load_state(out, stem)
    old_files = {item["place"]: item for item in state["files"]} if state else {}
    content = sorted((item["place"], item["sha256"]) for item in entries)
    old_content = sorted((item["place"], item["sha256"]) for item in old_files.values())
    version = previous_version(out, file_name, state)
    changed = state is None or content != old_content
    if changed:
        version = int(version) + 1

    # The copies the server sends, all in files/ under their bare names (the server finds a file by its bare name, wherever it is;
    # the FILE line alone says where the client puts it). Unchanged files keep the time they had (the client compares its file's
    # time with it), a changed or new file gets the time of this run, so that every client with an older copy takes the new one.
    copies = os.path.join(out, COPIES_FOLDER)
    os.makedirs(copies, exist_ok=True)
    for item in entries:
        target = os.path.join(copies, item["place"].rsplit("/", 1)[-1])
        old = old_files.get(item["place"])
        same = old is not None and old["sha256"] == item["sha256"]
        item["time"] = old["time"] if same else now
        if not (same and os.path.isfile(target) and sha256_of(target) == item["sha256"]):
            shutil.copyfile(item["source"], target)
        os.utime(target, (item["time"], item["time"]))
    removed = remove_unlisted(out, set(taken_elsewhere) | set(by_name))

    write_text(os.path.join(out, file_name),
               manifest_text(name, version, platform, mode, description, checksum, [], [item["place"] for item in entries]))
    save_state(out, stem, {"version": version, "files": [{key: item[key] for key in ("place", "sha256", "size", "time")}
                                                          for item in entries]})
    if name_in_base:
        update_base(out, base_name, add=[file_name])
    added = sum(1 for item in entries if item["place"] not in old_files)
    updated = sum(1 for item in entries if item["place"] in old_files and old_files[item["place"]]["sha256"] != item["sha256"])
    total = sum(item["size"] for item in entries)
    log(f"{file_name}: version {format_version(version)}{' (unchanged)' if not changed else ''}, {len(entries)} files, {total} bytes; "
        f"{added} new, {updated} changed, {len(old_files) - (len(entries) - added)} gone, {removed} old copies removed")
    return version


def remove_unlisted(out, wanted):
    """Deletes the copies in files/ that no package of the folder lists any more. Returns how many."""
    removed = 0
    copies = os.path.join(out, COPIES_FOLDER)
    for name in sorted(os.listdir(copies)) if os.path.isdir(copies) else []:
        if name not in wanted:
            os.remove(os.path.join(copies, name))
            removed += 1
    return removed


def update_base(out, base_name, add=None, drop=None, keep_only=None):
    """Writes basepackage.gupd: a SUBPACKAGE line for every package of the folder. Its version goes up when that list changed."""
    state = load_state(out, "basepackage") or {"version": previous_version(out, BASE_PACKAGE, None), "packages": [], "name": None}
    packages = [package for package in state["packages"] if package != drop]
    packages += [package for package in add or [] if package not in packages]
    packages = [package for package in packages if os.path.exists(os.path.join(out, package))
                and (keep_only is None or package in keep_only)]
    version = state["version"]
    if packages != state["packages"] or not os.path.exists(os.path.join(out, BASE_PACKAGE)):
        version = int(version) + 1
    write_text(os.path.join(out, BASE_PACKAGE), manifest_text(base_name, version, "any", "optional", [], False, packages, []))
    save_state(out, "basepackage", {"version": version, "packages": packages, "name": base_name})
    return version


def remove(out, file_name, base_name=None, log=print, name_in_base=True):
    """Takes a package out of the folder: its manifest, its copies and its state; the base package stops naming it."""
    stem = check_package_file_name(file_name)
    if not os.path.exists(os.path.join(out, file_name)):
        raise PackageError(f"{file_name}: not in {out}")
    os.remove(os.path.join(out, file_name))
    state_path = os.path.join(out, STATE_FOLDER, stem + ".json")
    if os.path.exists(state_path):
        os.remove(state_path)
    remove_unlisted(out, set(other_packages_names(out, stem)))
    if name_in_base:
        base_state = load_state(out, "basepackage") or {}
        version = update_base(out, base_name or base_state.get("name") or "Packages", drop=file_name)
        log(f"removed {file_name}; basepackage.gupd is version {format_version(version)}")
    else:
        log(f"removed {file_name}")


def folder_lines(prefix="packages"):
    """The foldersconfig.txt lines that make the server find every manifest and every file of a package folder copied to
    <world>/<prefix>/: the same two lines for any packages (the copies are all in files/)."""
    prefix = prefix.strip("/")
    check_folder(prefix)
    base = prefix + "/" if prefix else ""
    return [f"file    {base}*.gupd", f"file    {base}{COPIES_FOLDER}/*"]


def merge_block(config_path, lines):
    """Puts the lines into foldersconfig.txt between the block's markers, replacing an earlier block; the rest stays."""
    existing, newline = [], "\n"
    if os.path.exists(config_path):
        with open(config_path, "rb") as file:
            data = file.read().decode("utf-8")
        existing = data.splitlines()
        newline = "\r\n" if "\r\n" in data else "\n"
    result, inside, placed = [], False, False
    for line in existing:
        if line.strip() == BLOCK_BEGIN:
            inside = True
            continue
        if inside:
            if line.strip() == BLOCK_END:
                inside = False
                result.extend([BLOCK_BEGIN, *lines, BLOCK_END])
                placed = True
            continue
        result.append(line)
    if inside:
        raise PackageError(f"{config_path}: the block has a start line but no end line ({BLOCK_END})")
    if not placed:
        if result and result[-1].strip():
            result.append("")
        result.extend([BLOCK_BEGIN, *lines, BLOCK_END])
    with open(config_path, "w", encoding="utf-8", newline="") as file:
        file.write(newline.join(result) + newline)


def indexed_names(world, config_lines, skip_prefix):
    """The bare names (lower case) the server's file index would hold for a world folder and foldersconfig lines, with their paths;
    the files below skip_prefix (the packages folder) are left out. The server reads a line as a type, then a path whose last part is
    a pattern for one folder."""
    names = {}
    skip = skip_prefix.strip("/").lower() + "/"
    for source_line in config_lines:
        line = source_line.split("#", 1)[0].strip()
        parts = line.split(None, 1)
        if len(parts) < 2:
            continue
        configured = parts[1].strip().replace("\\", "/")
        folder, _, pattern = configured.rpartition("/")
        directory = os.path.join(world, *folder.split("/")) if folder else world
        if not pattern or not os.path.isdir(directory):
            continue
        for name in os.listdir(directory):
            path = os.path.join(directory, name)
            relative = os.path.relpath(path, world).replace(os.sep, "/")
            if os.path.isfile(path) and fnmatch.fnmatch(name.lower(), pattern.lower()) and not relative.lower().startswith(skip):
                names.setdefault(name.lower(), relative)
    return names


def check_world(out, world, prefix="packages", config_lines=None):
    """Refuses a package folder whose files share a bare name with a file the server already finds elsewhere in its world: the
    server's index keeps one path per bare name, so one of the two would be sent for both."""
    if config_lines is None:
        config_path = os.path.join(os.path.dirname(os.path.normpath(world)), "config", "foldersconfig.txt")
        config_lines = []
        if os.path.exists(config_path):
            with open(config_path, encoding="utf-8") as file:
                config_lines = file.read().splitlines()
    indexed = indexed_names(world, config_lines, prefix)
    clashes = []
    for entry in sorted(os.listdir(os.path.join(out, STATE_FOLDER))) if os.path.isdir(os.path.join(out, STATE_FOLDER)) else []:
        if entry == "basepackage.json":
            continue
        with open(os.path.join(out, STATE_FOLDER, entry), encoding="utf-8") as file:
            for item in json.load(file).get("files", []):
                bare = item["place"].rsplit("/", 1)[-1]
                if bare in indexed:
                    clashes.append(f"{entry[:-5]}.gupd lists {bare}, and the world has {indexed[bare]}")
    for entry in os.listdir(out):
        if entry.endswith(".gupd") and entry in indexed:
            clashes.append(f"{entry}: the world has {indexed[entry]} too")
    if clashes:
        raise PackageError("files the server would mix up (one bare name, two files):\n  " + "\n  ".join(clashes))


def check(path, log=print):
    """Reads a manifest as the client does and reports what each line does and what the client or the server would refuse."""
    manifest = read_manifest(path)
    if manifest is None:
        raise PackageError(f"{path}: the first line is not {HEADER}: the client ignores the whole file")
    problems = []
    if manifest["mode"] not in MODES:
        problems.append(f"MODE {manifest['mode']}: the installer knows optional, required and base (anything else acts as optional "
                        "but cannot be uninstalled)")
    seen = {}
    for entry in manifest["files"]:
        folder, _, name = entry.rpartition("/")
        try:
            check_file_name(name.strip())
            check_folder(folder)
        except PackageError as error:
            problems.append(f"FILE {entry}: {error}")
        bare = name.strip().lower()
        if bare in seen:
            problems.append(f"FILE {entry}: the bare name {bare} is listed twice (also {seen[bare]})")
        seen[bare] = entry
    for line in manifest["ignored"]:
        problems.append(f"{line!r}: the client ignores this line" + (" (only the bare word counts)"
                                                                     if line.split(" ")[0] in ("USECHECKSUM", "PROTECTOVERWRITE",
                                                                                               "ISMAINEXECUTABLE") else ""))
    log(f"{os.path.basename(path)}: NAME {manifest['name']!r}, VERSION {format_version(manifest['version'])}, PLATFORM "
        f"{manifest['platform']}, MODE {manifest['mode']}, {len(manifest['subpackages'])} sub-packages, {len(manifest['files'])} files"
        f"{', USECHECKSUM' if manifest['usechecksum'] else ''}{', PROTECTOVERWRITE' if manifest['protectoverwrite'] else ''}")
    for problem in problems:
        log("problem: " + problem)
    return problems


PACKAGE_SETTINGS = "package.txt"
FOLDER_SETTINGS = "packages.txt"


def read_settings(path, allowed, repeated=()):
    """A settings file: "key = value" lines, '#' starts a comment line; the keys in repeated may come more than once."""
    settings = {key: [] for key in repeated}
    with open(path, encoding="utf-8") as file:
        for number, line in enumerate(file.read().splitlines(), 1):
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            key, separator, value = line.partition("=")
            key, value = key.strip(), value.strip()
            if not separator or key not in allowed:
                raise PackageError(f"{path}:{number}: expected one of {', '.join(allowed)} = value")
            if key in repeated:
                settings[key].append(value)
            elif key in settings:
                raise PackageError(f"{path}:{number}: {key} is given twice")
            else:
                settings[key] = value
    return settings


def build(source, out, now=None, log=print):
    """Makes every package of a source folder into a package folder: each sub-folder with a package.txt is one package (its other
    files are the package's, or those of the folder its "source" names), packages.txt at the top may give the base package's name, and a package of the package folder whose
    sub-folder is gone is removed. Returns the file names of the packages."""
    if not os.path.isdir(source):
        raise PackageError(f"{source}: no such folder")
    base_name = "Packages"
    if os.path.exists(os.path.join(source, FOLDER_SETTINGS)):
        base_name = read_settings(os.path.join(source, FOLDER_SETTINGS), ("base-name",)).get("base-name", base_name)
    os.makedirs(out, exist_ok=True)
    packages = []
    for entry in sorted(os.listdir(source)):
        folder = os.path.join(source, entry)
        settings_path = os.path.join(folder, PACKAGE_SETTINGS)
        if entry.startswith(".") or not os.path.isfile(settings_path):
            continue
        settings = read_settings(settings_path, ("file", "name", "mode", "platform", "place", "description", "exclude", "checksum",
                                                  "source"),
                                 repeated=("description", "exclude"))
        settings.setdefault("file", entry.lower() + ".gupd")
        if "name" not in settings:
            raise PackageError(f"{settings_path}: name = ... is missing")
        if settings.get("checksum", "no") not in ("yes", "no"):
            raise PackageError(f"{settings_path}: checksum is yes or no")
        if any(settings["file"] == other["file"] for _, other in packages):
            raise PackageError(f"{settings_path}: two folders make {settings['file']}")
        if "source" in settings:
            # The files are elsewhere (a folder the package.txt names, relative to its own folder or absolute).
            folder = os.path.normpath(os.path.join(folder, os.path.expanduser(settings["source"])))
            if not os.path.isdir(folder):
                raise PackageError(f"{settings_path}: source = {settings['source']}: no such folder")
        packages.append((folder, settings))
    if not packages:
        raise PackageError(f"{source}: no sub-folder with a {PACKAGE_SETTINGS}")
    made = [settings["file"] for _, settings in packages]
    # Packages that are gone go first, so that their files may move to another package.
    for entry in sorted(os.listdir(out)):
        if entry.endswith(".gupd") and entry != BASE_PACKAGE and entry not in made:
            remove(out, entry, base_name, log, name_in_base=False)
    # What every package will list, so that a file may move from one package to another in one run.
    planned = {}
    for folder, settings in packages:
        files, _ = gather(folder, [PACKAGE_SETTINGS, *settings["exclude"]])
        planned[settings["file"]] = {place_of(relative, settings.get("place", "auto")).rsplit("/", 1)[-1] for relative, _ in files}
    for folder, settings in packages:
        taken = {name: other for other, names in planned.items() if other != settings["file"] for name in names}
        make(out, settings["file"], settings["name"], folder, settings.get("mode", "optional"), settings.get("platform", "any"),
             settings["description"], settings.get("place", "auto"), settings.get("checksum") == "yes",
             [PACKAGE_SETTINGS, *settings["exclude"]], base_name, now, log, taken, name_in_base=False)
    update_base(out, base_name, add=made, keep_only=made)
    return made


def description_lines(arguments):
    if arguments.description_file:
        with open(arguments.description_file, encoding="utf-8") as file:
            return file.read().splitlines()
    return arguments.description.splitlines() if arguments.description else []


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    commands = parser.add_subparsers(dest="command", required=True)
    make_parser = commands.add_parser("make", help="write or bring up to date one package of a package folder")
    make_parser.add_argument("source", help="the folder whose files the package brings")
    make_parser.add_argument("--out", required=True, help="the package folder (it goes into the world folder as packages/)")
    make_parser.add_argument("--file", required=True, help="the package's file name, such as starter.gupd")
    make_parser.add_argument("--name", required=True, help="the name the window shows")
    make_parser.add_argument("--mode", default="optional", choices=MODES)
    make_parser.add_argument("--platform", default="any")
    make_parser.add_argument("--description", default="")
    make_parser.add_argument("--description-file")
    make_parser.add_argument("--place", default="auto", help="auto (the client's own place, the default), keep (the path in the "
                                                             "source folder) or a folder such as levels/images/mine/")
    make_parser.add_argument("--checksum", action="store_true", help="write USECHECKSUM: the client compares checksums, not times")
    make_parser.add_argument("--exclude", action="append", default=[], help="a pattern of names or paths to leave out")
    make_parser.add_argument("--base-name", help="the base package's NAME (default: the one it has, else Packages)")
    build_parser = commands.add_parser("build", help="make every package of a source folder (a package.txt per sub-folder)")
    build_parser.add_argument("source")
    build_parser.add_argument("--out", required=True)
    remove_parser = commands.add_parser("remove", help="take a package out of a package folder")
    remove_parser.add_argument("--out", required=True)
    remove_parser.add_argument("--file", required=True)
    lines_parser = commands.add_parser("foldersconfig", help="print (or merge) the foldersconfig.txt lines of a package folder")
    lines_parser.add_argument("--out", help="a package folder to check against --world")
    lines_parser.add_argument("--prefix", default="packages", help="where the folder sits in the world folder (default packages)")
    lines_parser.add_argument("--merge", help="a foldersconfig.txt to write the lines into, between the block's markers")
    lines_parser.add_argument("--world", help="a world folder to check for files with the same bare names as the packages'")
    check_parser = commands.add_parser("check", help="read a .gupd as the client does and report problems")
    check_parser.add_argument("file")
    arguments = parser.parse_args(argv)
    try:
        if arguments.command == "make":
            os.makedirs(arguments.out, exist_ok=True)
            base_state = load_state(arguments.out, "basepackage") or {}
            make(arguments.out, arguments.file, arguments.name, arguments.source, arguments.mode, arguments.platform,
                 description_lines(arguments), arguments.place, arguments.checksum, arguments.exclude,
                 arguments.base_name or base_state.get("name") or "Packages")
        elif arguments.command == "build":
            build(arguments.source, arguments.out)
        elif arguments.command == "remove":
            remove(arguments.out, arguments.file)
        elif arguments.command == "foldersconfig":
            lines = folder_lines(arguments.prefix)
            if arguments.world:
                if not arguments.out:
                    raise PackageError("--world needs --out, the package folder to check")
                check_world(arguments.out, arguments.world, arguments.prefix)
            if arguments.merge:
                merge_block(arguments.merge, lines)
                print(f"wrote {len(lines)} lines into {arguments.merge}")
            else:
                print("\n".join(lines))
        else:
            return 1 if check(arguments.file) else 0
    except PackageError as error:
        print(f"gupd.py: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
