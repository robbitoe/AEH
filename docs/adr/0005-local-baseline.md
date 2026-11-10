# ADR-0005 — Base local simulada y límites de confianza

Estado: propuesta

## Contexto

La arquitectura objetivo contempla proveedores externos y PostgreSQL. MVP-1 debe validar el núcleo sin depender de sesiones reales ni de infraestructura compartida.

## Decisión propuesta

MVP-1 operaría con un solo operador local, repositorio de prueba y adaptador simulado. El worktree separa cambios de Git. La ejecución de procesos o contenido no confiable requiere un aislamiento adicional validado y se difiere. El simulador usa datos sintéticos y carece de acceso a credenciales y proveedores reales.

La política deniega permisos que no estén declarados. Acciones de alto riesgo quedan fuera de la simulación operativa. Para su eventual habilitación, la aprobación debe identificar al operador autorizado, acción, ejecución, versión de contrato y vencimiento. Un cambio en cualquiera de esos límites invalida la aprobación.

## Consecuencias

Pendiente elegir almacenamiento para MVP-1 con base en la capacidad de guardar de forma atómica cada transición y su evento, recuperar el registro tras reinicio y exportar evidencia. PostgreSQL sigue como candidato. El despliegue compartido, el mecanismo de aislamiento para ejecutores reales y su validación quedan pendientes antes de ampliar alcance.
