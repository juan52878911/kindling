"""Un agente mínimo: un modelo de Claude con la herramienta run_python.

Llama a la API de Claude (hace falta ANTHROPIC_API_KEY); `run.sh --dry-run` no llega aquí.
Necesita: pip install ./sdks/python langchain-core langchain-anthropic
"""
import sys

from langchain_anthropic import ChatAnthropic
from langchain_core.messages import HumanMessage, ToolMessage

import tool as sandbox_tool


def main(pregunta: str) -> None:
    llm = ChatAnthropic(model="claude-sonnet-5-5").bind_tools([sandbox_tool.run_python])
    mensajes = [HumanMessage(pregunta)]
    try:
        for _ in range(5):  # tope de vueltas: un agente sin tope es un gasto sin tope
            ia = llm.invoke(mensajes)
            mensajes.append(ia)
            if not ia.tool_calls:
                print(ia.content)
                return
            for llamada in ia.tool_calls:
                salida = sandbox_tool.run_python.invoke(llamada["args"])
                mensajes.append(ToolMessage(salida, tool_call_id=llamada["id"]))
        print("(sin respuesta final tras 5 vueltas)")
    finally:
        sandbox_tool.close()


if __name__ == "__main__":
    main(" ".join(sys.argv[1:]) or "What is the 30th Fibonacci number? Compute it, don't guess.")
