"""Una herramienta de LangChain que ejecuta código Python dentro de un sandbox de kindling.

No llama a ningún LLM: `python3 tool.py` la prueba sola. El agente (agent.py) la usa.
Necesita: pip install ./sdks/python langchain-core
"""
from kindling_sandbox import Sandbox
from langchain_core.tools import tool

# Un sandbox por proceso, creado la primera vez que el agente pide ejecutar algo.
_sb = None


def _sandbox() -> Sandbox:
    global _sb
    if _sb is None:
        _sb = Sandbox.create(template="python", ttl=900)
    return _sb


@tool
def run_python(code: str) -> str:
    """Run Python 3 code in an isolated microVM and return its stdout, stderr and exit code."""
    r = _sandbox().exec(["python3", "-c", code], timeout=30)
    return f"exit={r.exit_code}\nstdout:\n{r.stdout}\nstderr:\n{r.stderr}"


def close() -> None:
    global _sb
    if _sb is not None:
        _sb.delete()
        _sb = None


if __name__ == "__main__":
    try:
        print(run_python.invoke({"code": "print(6 * 7)"}))
    finally:
        close()
