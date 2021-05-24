import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { render } from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';

module('Integration | Component | project-input-variables', function(hooks) {
  setupRenderingTest(hooks);

  test('it renders', async function(assert) {

    await render(hbs`<ProjectInputVariables::List/>`);
    assert.equal(this.element.getElementsByClassName('project-input-variables-list').length, 1);
  });
});
