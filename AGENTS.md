# AGENTS.md — AEH

## Propósito

Este repositorio define y documenta **AEH (Autonomous Engineering Harness)**. En esta etapa el objetivo es fijar el problema, las decisiones arquitectónicas y las condiciones de validación antes de implementar funcionalidades.

## Reglas de trabajo

1. La documentación es el contrato de diseño hasta que una ADR aprobada la sustituya.
2. Toda afirmación no verificada debe marcarse como `Hipótesis`, `Pendiente` o `No expuesto`.
3. No asumir disponibilidad de tokens/cuotas cuando un proveedor autenticado por suscripción no la expone.
4. Separar siempre: intención de producto, contrato de ejecución, evidencia producida y resultado verificado.
5. Las decisiones que afecten seguridad, aislamiento, proveedores, persistencia o trazabilidad requieren una ADR.
6. No implementar integraciones reales en la fase documental/MVP-0.

## Entregables y flujo de Git

1. Planificar el trabajo por MVP o versión completa. Cada tarea tiene un alcance acotado y criterios de aceptación vinculados al entregable.
2. Usar `dev` como rama de integración y `main` para versiones completas aprobadas. Mantener `main` sin archivos del proyecto hasta cerrar el primer entregable. Git requiere un commit inicial vacío para materializar esa rama.
3. Antes de iniciar una tarea, actualizar referencias desde GitHub y crear su rama a partir de `origin/dev`. Usar un worktree separado por tarea cuando haya trabajo en paralelo. Paralelizar solo tareas independientes, sin solapamientos de código ni conflictos de arquitectura previstos.
4. Abrir un PR hacia `dev` al finalizar cada tarea, con alcance, criterios de aceptación y evidencia de validación. Cada PR requiere dos dictámenes independientes del autor, uno de un agente de QA y otro de un agente de seguridad, sobre la revisión exacta del cambio. Si cambia el contenido, renovar ambas revisiones antes de hacer merge. Registrar los dictámenes en el PR.
5. Integrar en `dev` mediante PR tras ambas aprobaciones. Al cerrar y aprobar la versión completa, promover `dev` a `main` mediante PR con las mismas revisiones.
6. Después de confirmar el merge y que el worktree está limpio, retirar el worktree y la rama local de la tarea. Si hay cambios locales o commits ajenos al PR, conservarlos y revisar antes de limpiar. Actualizar desde `origin/dev` para el siguiente cambio.
7. Aplicar las buenas prácticas relevantes al cambio. Preferir la solución más sencilla que satisfaga los requisitos de forma robusta y verificable, con trabajo adicional solo si el entregable lo requiere.

El repositorio de AEH es público y se distribuye bajo la licencia MIT. Revisar el contenido y los metadatos antes de publicarlos para evitar divulgar datos privados o credenciales.

## Lenguaje y nomenclatura

- Documentación principal: español.
- Código y nombres de componentes: inglés.
- `Outcome`: resultado observable que se desea lograr.
- `Execution Contract`: límites y condiciones bajo los que se ejecuta una fase.
- `Evidence`: artefactos verificables generados durante la ejecución.
- `Provider Adapter`: integración sustituible con Codex CLI, Claude Code u otro ejecutor.

## Criterio para cambiar documentos

Los cambios de alcance o arquitectura deben actualizar primero la documentación relevante y, si son decisiones estructurales, añadir o modificar una ADR. No se debe “resolver” una ambigüedad importante únicamente en código.
