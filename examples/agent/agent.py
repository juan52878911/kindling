#!/usr/bin/env python3
"""Agente mínimo: modelo local (ollama) + herramientas MCP en microVMs remotas.

Cierra el circuito completo de kindling:

    modelo local  ──>  gateway  ──>  microVM  ──>  servidor MCP
      (tu Mac)        (Proxmox)     (Firecracker)    (stdio o HTTP)

El modelo no sabe nada de microVMs: pide una herramienta y la herramienta
aparece. Si llevaba rato sin usarse estaba congelada, y despertarla cuesta
milisegundos.

    python3 agent.py "usa la herramienta echo para decir hola"
    python3 agent.py -s eco -m qwen3:4b "¿cuántas llamadas lleva la sesión?"
"""

import argparse
import json
import sys
import urllib.error
import urllib.request

PROTOCOL = "2025-06-18"


class MCPClient:
    """Cliente MCP sobre Streamable HTTP.

    El gateway enruta por Mcp-Session-Id, así que basta con conservar la cabecera
    que devuelve el initialize para que toda la conversación caiga siempre en la
    misma microVM. Sin eso, la segunda llamada podría ir a otra instancia y
    encontrarse un servidor sin el estado de esta sesión.
    """

    def __init__(self, url: str, timeout: int = 60):
        self.url = url.rstrip("/")
        self.timeout = timeout
        self.session = None
        self._id = 0

    def _rpc(self, method: str, params=None):
        self._id += 1
        payload = {"jsonrpc": "2.0", "id": self._id, "method": method}
        if params is not None:
            payload["params"] = params

        headers = {"Content-Type": "application/json"}
        if self.session:
            headers["Mcp-Session-Id"] = self.session

        req = urllib.request.Request(
            self.url, data=json.dumps(payload).encode(), headers=headers, method="POST"
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                sid = resp.headers.get("Mcp-Session-Id")
                if sid:
                    self.session = sid
                body = resp.read()
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"{method}: HTTP {e.code}: {e.read().decode()[:200]}") from None
        except urllib.error.URLError as e:
            raise RuntimeError(f"no alcanzo el gateway en {self.url}: {e.reason}") from None

        if not body:
            return {}
        msg = json.loads(body)
        if "error" in msg:
            raise RuntimeError(f"{method}: {msg['error'].get('message')}")
        return msg.get("result", {})

    def initialize(self):
        return self._rpc("initialize", {
            "protocolVersion": PROTOCOL,
            "capabilities": {},
            "clientInfo": {"name": "kindling-agent", "version": "1.0"},
        })

    def tools(self):
        return self._rpc("tools/list").get("tools", [])

    def call(self, name, arguments):
        res = self._rpc("tools/call", {"name": name, "arguments": arguments})
        # El contenido MCP es una lista de bloques; para un modelo basta el texto.
        return "\n".join(
            c.get("text", "") for c in res.get("content", []) if c.get("type") == "text"
        ) or json.dumps(res)


def ollama(model, messages, tools, host, timeout=180):
    payload = {"model": model, "messages": messages, "stream": False}
    if tools:
        payload["tools"] = tools
    req = urllib.request.Request(
        f"{host.rstrip('/')}/api/chat",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())["message"]
    except urllib.error.URLError as e:
        raise RuntimeError(f"no alcanzo ollama en {host}: {e.reason}") from None


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("prompt", nargs="+")
    ap.add_argument("-g", "--gateway", default="http://192.168.2.60:8080")
    ap.add_argument("-s", "--service", default="eco")
    ap.add_argument("-m", "--model", default="qwen3:4b")
    ap.add_argument("-o", "--ollama", default="http://127.0.0.1:11434")
    ap.add_argument("--max-steps", type=int, default=6)
    args = ap.parse_args()

    url = f"{args.gateway.rstrip('/')}/mcp/{args.service}/mcp"
    mcp = MCPClient(url)

    info = mcp.initialize()
    server = info.get("serverInfo", {})
    print(f"→ {server.get('name','?')} v{server.get('version','?')}  sesión {mcp.session[:8]}",
          file=sys.stderr)

    mcp_tools = mcp.tools()
    print(f"→ herramientas: {', '.join(t['name'] for t in mcp_tools)}\n", file=sys.stderr)

    # Traducción del esquema MCP al formato de tool-calling de ollama.
    tools = [{
        "type": "function",
        "function": {
            "name": t["name"],
            "description": t.get("description", ""),
            "parameters": t.get("inputSchema", {"type": "object", "properties": {}}),
        },
    } for t in mcp_tools]

    messages = [
        {"role": "system", "content":
         "Eres un asistente con acceso a herramientas remotas. Úsalas cuando "
         "aporten información que no tienes. Responde en español y de forma breve."},
        {"role": "user", "content": " ".join(args.prompt)},
    ]

    for step in range(args.max_steps):
        msg = ollama(args.model, messages, tools, args.ollama)
        messages.append(msg)

        calls = msg.get("tool_calls") or []
        if not calls:
            print(msg.get("content", "").strip())
            return 0

        for c in calls:
            fn = c["function"]
            name = fn["name"]
            raw_args = fn.get("arguments") or {}
            if isinstance(raw_args, str):
                raw_args = json.loads(raw_args or "{}")
            print(f"→ llamando {name}({json.dumps(raw_args, ensure_ascii=False)})", file=sys.stderr)
            try:
                result = mcp.call(name, raw_args)
            except RuntimeError as e:
                result = f"error: {e}"
            print(f"← {result}", file=sys.stderr)
            messages.append({"role": "tool", "content": result})

    print("(agotados los pasos sin respuesta final)", file=sys.stderr)
    return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except RuntimeError as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
