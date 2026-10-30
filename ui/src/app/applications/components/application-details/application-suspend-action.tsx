import {NotificationType} from 'argo-ui';
import * as React from 'react';
import {ErrorNotification} from '../../../shared/components';
import {ContextApis} from '../../../shared/context';
import * as models from '../../../shared/models';
import {services} from '../../../shared/services';

export function isAppSuspended(app: models.Application): boolean {
    return app.metadata.annotations?.[models.AnnotationSuspendKey] === 'true';
}

export function suspendActionLabel(app: models.Application): {title: string; iconClassName: string} {
    return isAppSuspended(app) ? {title: 'Resume', iconClassName: 'fa fa-play'} : {title: 'Suspend', iconClassName: 'fa fa-pause'};
}

export async function toggleAppSuspend(app: models.Application, apis: ContextApis): Promise<void> {
    const suspended = isAppSuspended(app);
    const confirmationTitle = suspended ? 'Resume Application?' : 'Suspend Application?';
    const confirmed = await apis.popup.confirm(
        confirmationTitle,
        suspended
            ? 'Are you sure you want to resume the application? Its workloads will return to the state declared in Git.'
            : 'Are you sure you want to suspend the application? Its workloads will be scaled down or paused until it is resumed.'
    );
    if (!confirmed) {
        return;
    }
    try {
        await services.applications.runResourceAction(
            app.metadata.name,
            app.metadata.namespace,
            {
                name: app.metadata.name,
                namespace: app.metadata.namespace,
                group: 'argoproj.io',
                kind: 'Application',
                version: 'v1alpha1'
            } as models.ResourceNode,
            'toggle-suspend',
            []
        );
    } catch (e) {
        apis.notifications.show({
            content: <ErrorNotification title={`Unable to ${suspended ? 'resume' : 'suspend'} application`} e={e} />,
            type: NotificationType.Error
        });
    }
}
