import * as models from '../../../shared/models';
import {services} from '../../../shared/services';
import {ContextApis} from '../../../shared/context';
import {isAppSuspended, suspendActionLabel, toggleAppSuspend} from './application-suspend-action';

jest.mock('../../../shared/services', () => ({
    services: {
        applications: {
            runResourceAction: jest.fn(() => Promise.resolve([]))
        }
    }
}));

const runResourceAction = services.applications.runResourceAction as jest.Mock;

function newApp(annotations?: {[key: string]: string}): models.Application {
    return {metadata: {name: 'guestbook', namespace: 'argocd', annotations}} as unknown as models.Application;
}

function newApis(confirmed: boolean): ContextApis {
    return {
        popup: {confirm: jest.fn(() => Promise.resolve(confirmed))},
        notifications: {show: jest.fn()}
    } as unknown as ContextApis;
}

describe('isAppSuspended', () => {
    it('is true only when the suspend annotation is "true"', () => {
        expect(isAppSuspended(newApp())).toBe(false);
        expect(isAppSuspended(newApp({[models.AnnotationSuspendKey]: 'false'}))).toBe(false);
        expect(isAppSuspended(newApp({[models.AnnotationSuspendKey]: 'true'}))).toBe(true);
    });
});

describe('suspendActionLabel', () => {
    it('offers Suspend for a running application', () => {
        expect(suspendActionLabel(newApp())).toEqual({title: 'Suspend', iconClassName: 'fa fa-pause'});
    });

    it('offers Resume for a suspended application', () => {
        expect(suspendActionLabel(newApp({[models.AnnotationSuspendKey]: 'true'}))).toEqual({title: 'Resume', iconClassName: 'fa fa-play'});
    });
});

describe('toggleAppSuspend', () => {
    beforeEach(() => {
        runResourceAction.mockReset();
        runResourceAction.mockResolvedValue([]);
    });

    it('runs the toggle-suspend action on the Application after confirmation', async () => {
        const apis = newApis(true);
        await toggleAppSuspend(newApp(), apis);

        expect(apis.popup.confirm).toHaveBeenCalledWith('Suspend Application?', expect.any(String));
        expect(runResourceAction).toHaveBeenCalledWith(
            'guestbook',
            'argocd',
            {name: 'guestbook', namespace: 'argocd', group: 'argoproj.io', kind: 'Application', version: 'v1alpha1'},
            'toggle-suspend',
            []
        );
    });

    it('asks to resume a suspended application', async () => {
        const apis = newApis(true);
        await toggleAppSuspend(newApp({[models.AnnotationSuspendKey]: 'true'}), apis);

        expect(apis.popup.confirm).toHaveBeenCalledWith('Resume Application?', expect.any(String));
        expect(runResourceAction).toHaveBeenCalledTimes(1);
    });

    it('does nothing when the confirmation is declined', async () => {
        await toggleAppSuspend(newApp(), newApis(false));

        expect(runResourceAction).not.toHaveBeenCalled();
    });

    it('shows an error notification when the action fails', async () => {
        runResourceAction.mockRejectedValue(new Error('permission denied'));
        const apis = newApis(true);
        await toggleAppSuspend(newApp(), apis);

        expect(apis.notifications.show).toHaveBeenCalledWith(expect.objectContaining({type: expect.anything()}));
    });
});
