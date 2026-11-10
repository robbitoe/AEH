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

## Recorridos documentales para la revisión

1. **Éxito.** El coordinador fija contratos y ejecuta el simulador. El verificador comprueba el artefacto y emite `pass`. Solo se cierra con todos los criterios satisfechos.
2. **Capacidad desconocida.** Sin regla explícita, el coordinador registra `unknown` y bloquea antes de invocar. La traza conserva la medición y la decisión por separado.
3. **Efecto incierto.** Tras un timeout, se conserva el intento. El operador revisa el checkpoint y el coordinador revalida antes de autorizar otro intento.
4. **Evidencia ausente.** El verificador emite `needs_review` aunque el adaptador haya completado su invocación.
