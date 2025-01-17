import { Factory, trait, association } from 'ember-cli-mirage';
import { fakeId, sequenceRandom } from '../utils';

export default Factory.extend({
  afterCreate(release, server) {
    if (!release.workspace) {
      let workspace =
        server.schema.workspaces.findBy({ name: 'default' }) || server.create('workspace', 'default');
      release.update('workspace', workspace);
    }
  },

  random: trait({
    id: () => fakeId(),
    component: association('release-manager', 'with-random-name'),
    sequence: () => sequenceRandom(),
    status: association('random'),
    state: 'CREATED',
    labels: () => ({
      'common/vcs-ref': '0d56a9f8456b088dd0e4a7b689b842876fd47352',
      'common/vcs-ref-path': 'https://github.com/hashicorp/waypoint/commit/',
    }),
    url: 'https://wildly-intent-honeybee.waypoint.run',
  }),

  'seconds-old-success': trait({
    status: association('random', 'success', 'seconds-old'),
  }),

  'minutes-old-success': trait({
    status: association('random', 'success', 'minutes-old'),
  }),

  'hours-old-success': trait({
    status: association('random', 'success', 'hours-old'),
  }),

  'days-old-success': trait({
    status: association('random', 'success', 'days-old'),
  }),
});
