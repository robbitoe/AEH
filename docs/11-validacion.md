# 11. Matriz de validación

Los roles son propuestos. Cada comprobación futura debe conservar el resultado y la referencia de evidencia. Los recorridos documentales pueden hacerse con casos de papel durante MVP-0.

| ID | Hipótesis o pendiente | Fase y rol | Evidencia y decisión |
|---|---|---|---|
| H-01 | Interfaces públicas y sesiones existentes permiten ejecución supervisada. | MVP-2, integrador | Revisar superficies por herramienta y versión. Registrar modos admitidos y excluir superficies sin soporte verificable. |
| H-02 | Existen señales permitidas de límite, sesión y error. Cuota restante: no expuesta a AEH hasta demostrar una medición verificable. | MVP-2, integrador | Registrar señales redactadas con origen y versión. Sin medición fiable, conservar `unknown`. |
| H-03 | La clasificación del adaptador permite decidir pausa, fallo o fallback. | MVP-1, responsable del núcleo | Simular timeout, cancelación y error ambiguo. El error ambiguo conserva su causa como no clasificada y evita fallback automático. |
| H-04 | Un único operador y repositorios de prueba bastan para MVP-1. | MVP-0, responsable de producto | Aprobar alcance en ADR-0005. Validar aislamiento antes de incorporar contenido o procesos no confiables. |
| H-05 | El almacenamiento elegido preserva transición y evento tras reinicio. | Decisión en MVP-0, validación en MVP-1, responsable del núcleo | Comparar candidatos contra ADR-0005 y comprobar recuperación en MVP-1. |
| H-06 | Un verificador determinista separado cubre los criterios del simulador. | MVP-0, verificador | Recorrer casos de evidencia válida, ausente e incompatible. Confirmar que solo la válida permite cerrar el criterio. |
| H-07 | Una estimación aporta valor operativo. | MVP-3, operador | Comparar estimaciones con datos de referencia si existen. Sin referencia verificable, mantener la estimación fuera de decisiones automáticas. |
| H-08 | Los modelos locales tienen un caso de uso viable. | Diferida, responsable de producto | Reabrir con un caso concreto y evidencia sobre hardware, licencia y aislamiento. |

## Resultados de los recorridos documentales

El responsable del proyecto confirmó los cuatro resultados esperados mediante una revisión sobre el papel. La tabla registra esa revisión. Las pruebas del simulador corresponden a MVP-1.

| Caso | Entrada o situación revisada | Resultado confirmado |
|---|---|---|
| 1. Éxito verificado | El único criterio exige un `report.json` con los campos `id` y `status`. | Con ambos campos, el verificador separado registra `pass` y el coordinador cierra la ejecución como `succeeded`. Si el archivo existe y falta `status`, el verificador registra `fail` y el coordinador aplica la política de recuperación. |
| 2. Capacidad desconocida | Una fase requiere más del 10 % de cuota. Falta una medición fiable y el contrato carece de una regla para ese caso. | El coordinador registra `unknown` como capacidad y `blocked` como decisión de política. La traza deja constancia de que el proveedor no fue invocado. |
| 3. Timeout con efectos inciertos | Un intento supera el límite de tiempo y deja un `report.json` parcial en el worktree. | Tras confirmar la detención, el coordinador registra el `timeout`, conserva el intento y la evidencia parcial, y pausa. El operador revisa los efectos. El coordinador autoriza otro intento solo si el checkpoint permite repetir sin duplicarlos y quedan intentos. Si persiste la incertidumbre, la ejecución sigue pausada. |
| 4. Evidencia ausente | El adaptador termina con código `0`, pero falta `report.json` en la ubicación acordada. | El verificador registra qué esperaba y dónde lo buscó, y emite el dictamen `needs_review`. El coordinador deja la ejecución en `needs_review` y el outcome queda pendiente. |
