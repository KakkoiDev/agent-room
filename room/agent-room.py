#!/usr/bin/env python3
"""agent-room helper: lets an AI agent join a chat room of people and agents.

Connects an agent to one room. It only talks to the server in the invite link
and keeps its login token in ~/.local/state/agent-room/.

Typical use:
  check    confirm this machine can reach the room
  join     join the room under NAME
  listen   wait until someone @mentions you, print the request, then exit
  reply    answer a request (progress and question keep it open)
  done     close a request that needs no answer, without posting
  run      for a local worker script: keep listening and run it per request

Exit codes: 0 done (or a request arrived), 1 error, 3 a person asked you to stop.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, quote, urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

EXIT_STOPPED = 3
LOCAL_HOSTS = {"localhost", "127.0.0.1", "::1"}


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Stopped(Exception):
    """A person in the room stopped this agent (HTTP 410)."""


class Fatal(Exception):
    """An error that retrying will not fix."""


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("invite", help="The chat link from the invite")
    parser.add_argument("name", help="Your agent name in this room; use the same one in every command")
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("check", help="Check that this machine can reach the room")
    commands.add_parser("join", help="Join the room and show a short summary")
    commands.add_parser("read", help="Print the whole room as JSON and mark it read")
    wait = commands.add_parser("wait", help="Wait up to 60 seconds for new messages (JSON)")
    wait.add_argument("--after", type=int, help="Defaults to the last read revision")
    listen = commands.add_parser("listen", help="Wait until a request arrives, print it, and exit")
    listen.add_argument("--timeout", type=int, default=0,
                        help="Give up after this many seconds with 'No request yet' (default: wait forever)")
    listen.add_argument("--context", type=int, default=8, help="Recent messages to show with a request")
    post = commands.add_parser("post", help="Post to the room while you have an open request")
    post.add_argument("revision", type=int)
    post.add_argument("text", nargs="?")
    post.add_argument("--file", help="Read the text from a file, or - for stdin")
    for action in ("reply", "progress", "question", "review", "handoff"):
        command = commands.add_parser(action)
        command.add_argument("assignment", help="The request ID")
        if action in ("review", "handoff"):
            command.add_argument("recipient", help="The agent to hand the work to")
        command.add_argument("text", nargs="?")
        command.add_argument("--file", help="Read the text from a file, or - for stdin")
        command.add_argument("--request-id", help="Reuse this ID when retrying the same message")
    done = commands.add_parser("done", help="Close a request that needs no answer, without posting")
    done.add_argument("assignment", help="The request ID")
    run = commands.add_parser("run", help="Keep listening and run a local worker for each request")
    run.add_argument("worker", nargs=argparse.REMAINDER, help="Worker executable and arguments, after --")
    args = parser.parse_args()
    if args.command == "run":
        args.worker = args.worker[1:] if args.worker[:1] == ["--"] else args.worker
        if not args.worker:
            parser.error("Specify a worker executable after run --.")

    invite = urlsplit(args.invite)
    room = parse_qs(invite.query).get("room", ["general"])[0]
    secure = invite.scheme == "https" or (invite.scheme == "http" and invite.hostname in LOCAL_HOSTS)
    if (not secure or not invite.hostname or invite.username is not None
            or not invite.path.endswith("/chat") or not room or "/" in room):
        parser.error("Use the chat link from the invite (https://…/chat?room=…).")
    base_path = invite.path[: -len("/chat")]
    if not args.name.strip() or len(args.name) > 80:
        parser.error("Choose a name of 1 to 80 characters.")

    client = Client(f"{invite.scheme}://{invite.netloc}{base_path}", room, args.name.strip(), args.invite)
    try:
        return COMMANDS[args.command](client, args)
    except Stopped as stop:
        print(f"STOPPED: {stop}", flush=True)
        return EXIT_STOPPED
    except (Fatal, ValueError, RuntimeError, OSError) as error:
        print(f"ERROR: {describe(error, client.host)}", file=sys.stderr, flush=True)
        return 1
    except KeyboardInterrupt:
        return 0


class Client:
    def __init__(self, origin, room, name, invite):
        self.origin, self.room, self.name, self.invite = origin, room, name, invite
        self.host = urlsplit(origin).hostname
        self.api = f"{origin}/api/chat/rooms/{quote(room, safe='')}"
        state_dir = Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local/state")) / "agent-room"
        state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.state_dir = state_dir
        self.state_file = state_dir / hashlib.sha256(f"{self.api}\n{name}".encode()).hexdigest()
        try:
            self.state = json.loads(self.state_file.read_text())
        except (OSError, ValueError):
            self.state = {"revision": 0}
        self.opener = build_opener(NoRedirect())

    def save(self):
        with tempfile.NamedTemporaryFile(mode="w", dir=self.state_dir, delete=False) as handle:
            temp_path = Path(handle.name)
            try:
                json.dump(self.state, handle)
                handle.flush()
                temp_path.replace(self.state_file)
            finally:
                temp_path.unlink(missing_ok=True)

    def request(self, suffix="", data=None, url=None, auth=True):
        headers = {"Accept": "application/json", "User-Agent": "agent-room"}
        if auth and self.state.get("token"):
            headers["Authorization"] = "Bearer " + self.state["token"]
        payload = None
        if data is not None:
            headers["Content-Type"] = "application/json"
            payload = json.dumps(data).encode()
        try:
            with self.opener.open(Request(url or self.api + suffix, data=payload, headers=headers), timeout=75) as response:
                body = response.read()
                return json.loads(body) if body else None
        except HTTPError as error:
            if error.code == 410:
                raise Stopped(error.read(1024).decode(errors="replace").strip()) from None
            raise

    def join(self):
        try:
            result = self.request("/participants", {"username": self.name, "role": "agent"}, auth=False)
        except HTTPError as error:
            if error.code == 409:
                raise Fatal(error.read(1024).decode(errors="replace").strip()) from None
            raise
        self.state["token"] = result["token"]
        self.state["revision"] = 0
        self.save()
        return bool(result.get("reclaimed"))

    def call(self, suffix="", data=None):
        """Authenticated call that joins (again) when the session is missing or expired."""
        if not self.state.get("token"):
            self.join()
        try:
            return self.request(suffix, data)
        except HTTPError as error:
            if error.code != 401:
                raise
            self.join()
            return self.request(suffix, data)

    def receipt(self, revision):
        if revision != self.state.get("revision"):
            self.call("/read", {"revision": revision})
            self.state["revision"] = revision
            self.save()

    def wait(self, after, timeout=60):
        return self.call("/wait?" + urlencode({"after": after, "timeout": timeout, "work": 1}))

    def cmd(self, words):
        return f'python3 agent-room.py "{self.invite}" "{self.name}" {words}'


def transient(error):
    if isinstance(error, HTTPError):
        return error.code >= 500 or error.code == 429
    if isinstance(error, URLError):
        return not isinstance(error.reason, socket.gaierror)
    return isinstance(error, (TimeoutError, ConnectionError, socket.timeout))


def describe(error, host):
    if isinstance(error, HTTPError):
        detail = error.read(1024).decode(errors="replace").strip()
        if error.code == 403:
            return (f"{host} refused this machine ({detail}). "
                    "Ask the room's owner to give you access.")
        if error.code == 404:
            return "This chat room does not exist on the server. Check the invite link."
        return f"The server answered HTTP {error.code}: {detail}"
    if isinstance(error, URLError):
        if isinstance(error.reason, socket.gaierror):
            return (f"Cannot find {host}. This machine is not on the network that hosts the room "
                    "(for example its Tailscale network; cloud sandboxes usually are not). Tell your user; retrying will not help.")
        return f"Cannot connect to {host}: {error.reason}. Check your network connection (VPN or Tailscale) and that the room's server is running."
    return str(error)


def read_text(args):
    if args.file and args.text is not None:
        raise Fatal("Supply the text or --file, not both.")
    if args.file == "-":
        text = sys.stdin.read()
    elif args.file:
        text = Path(args.file).read_text()
    elif args.text is not None:
        text = args.text
    else:
        text = sys.stdin.read()
    if not text.strip():
        raise Fatal("The message is empty.")
    return text


def short(text, limit=600):
    text = (text or "").strip()
    return text if len(text) <= limit else text[:limit] + " […]"


def speaker(message):
    if message.get("author"):
        return message["author"] + (" (agent)" if message.get("kind") == "agent" else "")
    return {"task": "Shared task"}.get(message.get("kind"), "Guest")


def cmd_check(client, args):
    guide = client.request(url=client.origin + "/api/chat", auth=False)
    if not isinstance(guide, dict):
        raise Fatal("The server did not answer like an agent-room server.")
    room = client.request(auth=False)
    people = client.request("/participants", auth=False) or []
    title = (room.get("room") or {}).get("title", client.room)
    print(f'OK: this machine can reach the room "{title}" '
          f"({len(people)} member{'s' if len(people) != 1 else ''}, {room.get('revision', 0)} messages).")
    print("Next: " + client.cmd("join"))
    return 0


def cmd_join(client, args):
    reclaimed = client.join()
    room = client.request()
    title = (room.get("room") or {}).get("title", client.room)
    messages = room.get("messages", [])
    client.receipt(room.get("revision", 0))
    print(f'Joined "{title}" as {client.name}. You can now receive requests.')
    if messages:
        print(f"\nLast {min(5, len(messages))} of {len(messages)} messages:")
        for m in messages[-5:]:
            if m.get("text"):
                print(f"- {speaker(m)}: {short(m['text'], 300)}")
    pending = room.get("assignments", [])
    if reclaimed:
        print(f"\nNote: an earlier agent in this room used the name {client.name}. You took it over, "
              "with its open requests below. Choose another name if you are not that agent.")
    print(f"\nOpen requests for you: {len(pending)}.")
    print("Next: " + client.cmd("listen"))
    return 0


def cmd_read(client, args):
    result = client.call()
    client.receipt(result["revision"])
    print(json.dumps(result, ensure_ascii=False, indent=2), flush=True)
    return 0


def cmd_wait(client, args):
    after = args.after if args.after is not None else client.state.get("revision", 0)
    for attempt in range(3):
        try:
            result = client.wait(after)
            break
        except (HTTPError, URLError, OSError) as error:
            if attempt == 2 or not transient(error):
                raise
            time.sleep(2 ** attempt * 2)
    client.receipt(result["revision"])
    print(json.dumps(result, ensure_ascii=False, indent=2), flush=True)
    return 0


def cmd_listen(client, args):
    deadline = time.monotonic() + args.timeout if args.timeout > 0 else None
    delay = 2
    while True:
        remaining = 60 if deadline is None else int(deadline - time.monotonic())
        if remaining <= 0:
            print("No request yet. Run listen again to keep listening: " + client.cmd("listen"), flush=True)
            return 0
        try:
            update = client.wait(client.state.get("revision", 0), min(60, remaining))
            delay = 2
        except (HTTPError, URLError, OSError) as error:
            if not transient(error):
                raise
            print(f"Connection problem ({describe(error, client.host)}). Retrying in {delay}s.", file=sys.stderr, flush=True)
            time.sleep(delay)
            delay = min(delay * 2, 60)
            continue
        client.receipt(update["revision"])
        work = update.get("assignments") or []
        if work:
            print_requests(client, work, args.context)
            return 0


def print_requests(client, work, context):
    room = client.call()
    messages = room.get("messages", [])
    title = (room.get("room") or {}).get("title", client.room)
    print(f'Room "{title}": {len(work)} open request{"s" if len(work) != 1 else ""} for {client.name}.\n')
    for item in work:
        source = messages[item["source"]] if 0 <= item.get("source", -1) < len(messages) else {}
        print(f"Request ID: {item['id']}")
        print(f"From: {speaker(source)}")
        print("Message:")
        print((item.get("prompt") or source.get("text") or "").strip())  # in full; the server bounds it
        if item.get("attachment"):
            print(f"Attachment (on the room's server): {item['attachment']}")
        print()
    recent = [m for m in messages if m.get("text")][-context:] if context > 0 else []
    if recent:
        print("Recent conversation, oldest first:")
        for m in recent:
            print(f"- {speaker(m)}: {short(m['text'], 400)}")
        print()
    first = work[0]["id"]
    print("Next: do the work, write your answer to a file, then send it with")
    print("  " + client.cmd(f"reply {first} --file answer.txt"))
    print("Use progress instead of reply for an update that keeps the request open,")
    print("or done to close a request that needs no answer (for example a status note).")
    print("@mention another agent in your answer only to hand it work. Then run listen again.")


def cmd_post(client, args):
    text = read_text(args)
    client.call(data={"revision": args.revision, "text": text})
    print("Posted. Run listen to wait for the next request.")
    return 0


def cmd_action(client, args):
    text = read_text(args)
    body = {"assignment_id": args.assignment, "action": args.command, "text": text,
            "agent": getattr(args, "recipient", "")}
    # Stable for identical retries; --request-id marks an intentional repeat.
    body["request_id"] = args.request_id or hashlib.sha256(json.dumps(body, sort_keys=True).encode()).hexdigest()
    client.call("/assignments", body)
    if args.command == "reply":
        print("Reply sent; that request is closed. Next: " + client.cmd("listen"))
    else:
        print(f"{args.command.capitalize()} sent; the request stays open.")
    return 0


def cmd_done(client, args):
    client.call("/assignments", {"assignment_id": args.assignment, "action": "done"})
    print("Request closed. Next: " + client.cmd("listen"))
    return 0


def cmd_run(client, args):
    while True:
        try:
            if client.state.get("outbox"):
                try:
                    client.call("/assignments", client.state["outbox"])
                except HTTPError as error:
                    if error.code not in (404, 409):
                        raise
                    print("Request is no longer active.", file=sys.stderr, flush=True)
                del client.state["outbox"]
                client.save()
            update = client.wait(client.state.get("revision", 0))
            client.receipt(update["revision"])
            if not update.get("assignments"):
                continue
            item = update["assignments"][0]
            try:
                client.call("/assignments", {"assignment_id": item["id"], "action": "working"})
            except HTTPError as error:
                if error.code == 409:
                    continue
                raise
            full = client.call()
            full["assignment"] = item
            action, reply = "reply", ""
            try:
                result = subprocess.run(args.worker, input=json.dumps(full), text=True,
                                        capture_output=True, timeout=600, check=False)
                if result.returncode:
                    detail = result.stderr.strip().splitlines()[-1:] or [""]
                    raise RuntimeError(f"Worker exited with status {result.returncode}. {detail[0]}".strip())
                reply = result.stdout.strip()
                if not reply or len(reply.encode()) > 12000:
                    raise RuntimeError("Worker returned an empty or oversized reply.")
            except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
                action, reply = "failed", str(error)[:2000]
            client.state["outbox"] = {"assignment_id": item["id"], "action": action,
                                      "text": reply, "request_id": str(uuid.uuid4())}
            client.save()
        except (HTTPError, URLError, OSError) as error:
            if not transient(error):
                raise
            print(f"Waiting to reconnect: {describe(error, client.host)}", file=sys.stderr, flush=True)
            time.sleep(5)


COMMANDS = {"check": cmd_check, "join": cmd_join, "read": cmd_read, "wait": cmd_wait, "listen": cmd_listen,
            "post": cmd_post, "run": cmd_run, "done": cmd_done, **{a: cmd_action for a in ("reply", "progress", "question", "review", "handoff")}}

if __name__ == "__main__":
    sys.exit(main())
